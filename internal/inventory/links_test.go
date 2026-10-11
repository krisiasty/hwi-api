package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"testing"
	"testing/fstest"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
)

type fakeLinks struct {
	links []Link
	err   error
}

func (f fakeLinks) Links() ([]Link, error) { return f.links, f.err }

func TestLinksProberReportsTheLinks(t *testing.T) {
	lister := fakeLinks{links: []Link{
		{Name: "eth0", Index: 2, MTU: 9000, AdminState: "up", OperState: "up", MACAddress: "aa:bb:cc:dd:ee:ff", Addresses: []string{"10.0.0.5/24"}},
	}}
	got, err := NewLinksProber(lister).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Links []Link `json:"links"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Links) != 1 || payload.Links[0].MTU != 9000 || payload.Links[0].OperState != "up" || payload.Links[0].Addresses[0] != "10.0.0.5/24" {
		t.Errorf("links = %+v, want the lister's single eth0 entry", payload.Links)
	}
}

// A host that genuinely reports no interfaces gets an empty list, not
// null: [] is a real, successful answer.
func TestLinksProberReportsAnEmptyListAsEmpty(t *testing.T) {
	got, err := NewLinksProber(fakeLinks{}).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	b, _ := json.Marshal(got)
	if string(b) != `{"links":[]}` {
		t.Errorf("payload = %s, want {\"links\":[]}", b)
	}
}

// TestLinksProberFailsInsteadOfReportingNoLinks checks that a failed
// enumeration is a probe failure, never an empty list, which a
// consumer reconciling inventory would read as every interface removed.
func TestLinksProberFailsInsteadOfReportingNoLinks(t *testing.T) {
	want := errors.New("netlink down")
	got, err := NewLinksProber(fakeLinks{err: want}).Probe(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("Probe error = %v, want the lister's error", err)
	}
	if got != nil {
		t.Errorf("Probe returned %v alongside its error, want nothing", got)
	}
}

// Through the store, a failure after an earlier success keeps the
// last-good list and records the error, which is what the API then serves
// as data plus errors.links.
func TestLinksFailureKeepsTheLastGoodListInTheStore(t *testing.T) {
	store := NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemLinks: true})
	schedules := []Schedule{{Subsystem: apiv1.SubsystemLinks, Interval: time.Minute}}

	good := fakeLinks{links: []Link{{Name: "eth0", Index: 2, MTU: 1500, AdminState: "up", OperState: "up"}}}
	first := NewRefresher(store, Probers{apiv1.SubsystemLinks: NewLinksProber(good)}, schedules, quietLogger(), nil)
	first.clock = newFakeClock(t0).Now
	first.RefreshOne(context.Background(), apiv1.SubsystemLinks, t0)

	failing := fakeLinks{err: errors.New("netlink down")}
	second := NewRefresher(store, Probers{apiv1.SubsystemLinks: NewLinksProber(failing)}, schedules, quietLogger(), nil)
	second.clock = newFakeClock(t0.Add(time.Minute)).Now
	second.RefreshOne(context.Background(), apiv1.SubsystemLinks, t0.Add(time.Minute))

	got, ok := store.Get(apiv1.SubsystemLinks)
	if !ok {
		t.Fatal("no links entry in the store")
	}
	if string(got.Body) != `{"links":[{"name":"eth0","index":2,"mtu":1500,"admin_state":"up","oper_state":"up"}]}` {
		t.Errorf("Body = %s, want the last-good list", got.Body)
	}
	if got.Err != "netlink down" {
		t.Errorf("Err = %q, want the failure recorded", got.Err)
	}
	if !got.CollectedAt.Equal(t0) {
		t.Errorf("CollectedAt = %s, want the last good collection time %s", got.CollectedAt, t0)
	}
}

func operstateFS(states map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, state := range states {
		fsys["sys/class/net/"+name+"/operstate"] = &fstest.MapFile{Data: []byte(state + "\n")}
	}
	return fsys
}

func noAddrs(net.Interface) ([]net.Addr, error) { return nil, nil }

// buildLinks reports the administrative state from the interface flags and
// passes the kernel's operational state through unchanged, so an
// interface that is enabled but has lost its carrier reads as up/down.
func TestBuildLinksReportsAdminAndOperState(t *testing.T) {
	ifaces := []net.Interface{
		{Index: 2, MTU: 1500, Name: "eno1", Flags: net.FlagUp | net.FlagBroadcast},
		{Index: 3, MTU: 1500, Name: "eno2"},
		{Index: 1, MTU: 65536, Name: "lo", Flags: net.FlagUp | net.FlagLoopback | net.FlagRunning},
	}
	fsys := operstateFS(map[string]string{"eno1": "down", "eno2": "down", "lo": "unknown"})

	got, err := buildLinks(ifaces, noAddrs, fsys)
	if err != nil {
		t.Fatalf("buildLinks: %v", err)
	}
	want := map[string][2]string{"eno1": {"up", "down"}, "eno2": {"down", "down"}, "lo": {"up", "unknown"}}
	if len(got) != len(want) {
		t.Fatalf("got %d links, want %d", len(got), len(want))
	}
	for _, l := range got {
		if w := want[l.Name]; l.AdminState != w[0] || l.OperState != w[1] {
			t.Errorf("%s: admin %q, oper %q, want %q, %q", l.Name, l.AdminState, l.OperState, w[0], w[1])
		}
	}
	if got[0].Name != "eno1" || got[2].Name != "lo" {
		t.Errorf("links are not sorted by name: %v", got)
	}
}

// Every RFC 2863 value the kernel can report passes through as is.
func TestBuildLinksPassesEveryOperStateThrough(t *testing.T) {
	for _, state := range []string{"up", "down", "lowerlayerdown", "dormant", "testing", "notpresent", "unknown"} {
		got, err := buildLinks([]net.Interface{{Name: "x0"}}, noAddrs, operstateFS(map[string]string{"x0": state}))
		if err != nil || len(got) != 1 || got[0].OperState != state {
			t.Errorf("operstate %q: got %v, %v", state, got, err)
		}
	}
}

// An operational state that cannot be read fails the whole list rather
// than being reported as "unknown", which is a real kernel value.
func TestBuildLinksFailsOnAnUnreadableOperState(t *testing.T) {
	_, err := buildLinks([]net.Interface{{Name: "eno1"}}, noAddrs, fstest.MapFS{})
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error = %v, want a failure naming the missing operstate", err)
	}
}

// Addresses that cannot be read fail the whole list rather than dropping
// that interface's addresses.
func TestBuildLinksFailsOnUnreadableAddresses(t *testing.T) {
	want := errors.New("netlink: address dump failed")
	_, err := buildLinks([]net.Interface{{Name: "eno1"}},
		func(net.Interface) ([]net.Addr, error) { return nil, want },
		operstateFS(map[string]string{"eno1": "up"}))
	if !errors.Is(err, want) {
		t.Errorf("error = %v, want the address lookup's error", err)
	}
}

func TestBuildLinksSortsEachInterfacesAddresses(t *testing.T) {
	addrs := func(net.Interface) ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
			&net.IPNet{IP: net.ParseIP("10.0.0.5"), Mask: net.CIDRMask(24, 32)},
		}, nil
	}
	got, err := buildLinks([]net.Interface{{Name: "eno1"}}, addrs, operstateFS(map[string]string{"eno1": "up"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0].Addresses) != 2 || got[0].Addresses[0] != "10.0.0.5/24" || got[0].Addresses[1] != "fe80::1/64" {
		t.Errorf("addresses = %v, want sorted CIDR strings", got[0].Addresses)
	}
}
