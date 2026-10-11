package helper

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

const devVPD = "sys/bus/pci/devices/0000:3b:00.0/vpd"

// hostRoot is a host root with two identity fields, a minimal SMBIOS table
// and one device's VPD, but no board_serial, as on many VMs.
func hostRoot() fstest.MapFS {
	return fstest.MapFS{
		"sys/class/dmi/id/product_uuid":   {Data: []byte("4c4c4544-0001-0002-0003-000000000004\n")},
		"sys/class/dmi/id/product_serial": {Data: []byte("EXSERIAL0001\n")},
		"sys/firmware/dmi/tables/DMI":     {Data: []byte{127, 4, 0, 0, 0, 0}},
		devVPD:                            {Data: []byte{0x82, 1, 0, 'X', 0x78}},
	}
}

func handle(t *testing.T, root fs.FS, req Request) Response {
	t.Helper()
	resp, fatal := Handler{Root: root, ReadTimeout: time.Second}.Handle(req)
	if fatal {
		t.Fatalf("Handle(%+v) reported a fatal read", req)
	}
	return resp
}

func TestHandleReadsEachDMIName(t *testing.T) {
	if resp := handle(t, hostRoot(), Request{Op: OpDMI, Name: "product_uuid"}); resp.Error != "" ||
		string(resp.Data) != "4c4c4544-0001-0002-0003-000000000004\n" {
		t.Errorf("product_uuid: %+v", resp)
	}
	if resp := handle(t, hostRoot(), Request{Op: OpDMI, Name: SMBIOSTable}); resp.Error != "" ||
		!bytes.Equal(resp.Data, []byte{127, 4, 0, 0, 0, 0}) {
		t.Errorf("DMI: %+v", resp)
	}
}

func TestHandleReadsVPDByAddress(t *testing.T) {
	resp := handle(t, hostRoot(), Request{Op: OpVPD, Address: "0000:3b:00.0"})
	if resp.Error != "" || !bytes.Equal(resp.Data, []byte{0x82, 1, 0, 'X', 0x78}) {
		t.Errorf("vpd: %+v", resp)
	}
}

// A file the host lacks is an answer, not a failure: the daemon then
// leaves the field out of the payload.
func TestHandleReportsAMissingFileAsNotFound(t *testing.T) {
	for _, req := range []Request{
		{Op: OpDMI, Name: "board_serial"},
		{Op: OpVPD, Address: "0000:5e:00.0"},
	} {
		if resp := handle(t, hostRoot(), req); resp.Error != ErrCodeNotFound {
			t.Errorf("%+v: %+v, want %s", req, resp, ErrCodeNotFound)
		}
	}
}

// The helper reads only what its operations name: never a path, a pattern
// or a file outside the DMI allowlist.
func TestHandleRefusesAnythingButItsOperations(t *testing.T) {
	for _, req := range []Request{
		{Op: "read", Name: "/etc/shadow"},
		{Op: OpDMI, Name: "../../../etc/shadow"},
		{Op: OpDMI, Name: "board_asset_tag"},
		{Op: OpDMI},
		{Op: OpVPD, Address: "0000:3b:00.0/../../../../etc"},
		{Op: OpVPD, Address: "3b:00.0"},
		{Op: OpVPD},
		{},
	} {
		if resp := handle(t, hostRoot(), req); resp.Error != ErrCodeInvalid || resp.Data != nil {
			t.Errorf("%+v: %+v, want %s", req, resp, ErrCodeInvalid)
		}
	}
}

func TestHandleCapsTheSize(t *testing.T) {
	root := hostRoot()
	root[devVPD] = &fstest.MapFile{Data: bytes.Repeat([]byte{0xff}, maxVPD+1)}
	if resp := handle(t, root, Request{Op: OpVPD, Address: "0000:3b:00.0"}); resp.Error != ErrCodeTooLarge || resp.Data != nil {
		t.Errorf("oversized VPD: %+v, want %s", resp.Error, ErrCodeTooLarge)
	}
	root[devVPD] = &fstest.MapFile{Data: bytes.Repeat([]byte{0xff}, maxVPD)}
	if resp := handle(t, root, Request{Op: OpVPD, Address: "0000:3b:00.0"}); resp.Error != "" || len(resp.Data) != maxVPD {
		t.Errorf("VPD at the cap: error %q, %d bytes", resp.Error, len(resp.Data))
	}
}

// Each kind of file has its own cap: an identity field's is far below the
// SMBIOS table's, and the table's below anything a request could make the
// helper buffer without bound.
func TestHandleCapsEachDMIName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
	}{
		{"product_serial", maxDMIField},
		{SMBIOSTable, maxSMBIOSTable},
	} {
		root := hostRoot()
		file, _ := DMIFile(tc.name)
		root[file] = &fstest.MapFile{Data: bytes.Repeat([]byte{'x'}, tc.limit+1)}
		if resp := handle(t, root, Request{Op: OpDMI, Name: tc.name}); resp.Error != ErrCodeTooLarge || resp.Data != nil {
			t.Errorf("%s over its cap: %+v, want %s", tc.name, resp.Error, ErrCodeTooLarge)
		}
		root[file] = &fstest.MapFile{Data: bytes.Repeat([]byte{'x'}, tc.limit)}
		if resp := handle(t, root, Request{Op: OpDMI, Name: tc.name}); resp.Error != "" || len(resp.Data) != tc.limit {
			t.Errorf("%s at its cap: error %q, %d bytes", tc.name, resp.Error, len(resp.Data))
		}
	}
}

// failingReadFS opens every file but fails its reads, as the kernel fails
// a VPD read the device does not answer properly.
type failingReadFS struct{ fs.FS }

func (f failingReadFS) Open(name string) (fs.File, error) {
	file, err := f.FS.Open(name)
	if err != nil {
		return nil, err
	}
	return failingReadFile{file}, nil
}

type failingReadFile struct{ fs.File }

func (failingReadFile) Read([]byte) (int, error) { return 0, errors.New("input/output error") }

func TestHandleReportsAFailedRead(t *testing.T) {
	resp := handle(t, failingReadFS{hostRoot()}, Request{Op: OpVPD, Address: "0000:3b:00.0"})
	if resp.Error != ErrCodeRead || resp.Data != nil || !strings.Contains(resp.Message, "input/output error") {
		t.Errorf("failed read: %+v, want %s naming the error", resp, ErrCodeRead)
	}
}

// hangingFS blocks every open of name until release is closed, as a VPD
// read stuck in the kernel does.
type hangingFS struct {
	fs.FS
	name    string
	release chan struct{}
}

func (h hangingFS) Open(name string) (fs.File, error) {
	if name == h.name {
		<-h.release
	}
	return h.FS.Open(name)
}

func TestHandleTimesOutAHungReadAndReportsItFatal(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	h := Handler{Root: hangingFS{hostRoot(), devVPD, release}, ReadTimeout: 50 * time.Millisecond}
	resp, fatal := h.Handle(Request{Op: OpVPD, Address: "0000:3b:00.0"})
	if resp.Error != ErrCodeTimeout || !fatal {
		t.Errorf("Handle = %+v, fatal %v; want %s and fatal", resp, fatal, ErrCodeTimeout)
	}
}

// serveInput runs Serve over input and returns its answers and its result.
func serveInput(t *testing.T, h Handler, input string) ([]Response, error) {
	t.Helper()
	var out bytes.Buffer
	err := h.Serve(struct {
		io.Reader
		io.Writer
	}{strings.NewReader(input), &out})
	var resps []Response
	dec := json.NewDecoder(&out)
	for {
		var r Response
		if err := dec.Decode(&r); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decoding an answer: %v", err)
		}
		resps = append(resps, r)
	}
	return resps, err
}

func TestServeAnswersEachRequestInTurn(t *testing.T) {
	resps, err := serveInput(t, Handler{Root: hostRoot(), ReadTimeout: time.Second},
		`{"op":"dmi","name":"product_serial"}`+"\n"+`{"op":"vpd","address":"0000:3b:00.0"}`+"\n")
	if err != nil {
		t.Fatalf("Serve: %v, want nil at the end of the input", err)
	}
	if len(resps) != 2 || string(resps[0].Data) != "EXSERIAL0001\n" || resps[1].Error != "" {
		t.Errorf("answers %+v", resps)
	}
}

// A malformed request ends the connection after one refusal: the request
// after it is never answered.
func TestServeStopsAtAMalformedRequest(t *testing.T) {
	next := `{"op":"dmi","name":"product_serial"}` + "\n"
	for name, input := range map[string]string{
		"not JSON":       "not json\n" + next,
		"unknown field":  `{"op":"dmi","name":"DMI","path":"/etc/shadow"}` + "\n" + next,
		"trailing data":  `{"op":"dmi","name":"DMI"} {"op":"dmi","name":"DMI"}` + "\n" + next,
		"too long":       `{"op":"dmi","name":"` + strings.Repeat("x", maxRequestBytes) + `"}` + "\n" + next,
		"cut off at EOF": `{"op":"dmi"`,
	} {
		resps, err := serveInput(t, Handler{Root: hostRoot(), ReadTimeout: time.Second}, input)
		if err == nil {
			t.Errorf("%s: Serve returned nil, want an error", name)
		}
		if len(resps) != 1 || resps[0].Error != ErrCodeInvalid {
			t.Errorf("%s: answers %+v, want one %s", name, resps, ErrCodeInvalid)
		}
	}
}

// After a hung read the helper answers, then stops: the goroutine stuck in
// the kernel ends only with the process.
func TestServeExitsAfterAHungRead(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	h := Handler{Root: hangingFS{hostRoot(), devVPD, release}, ReadTimeout: 50 * time.Millisecond}
	resps, err := serveInput(t, h, `{"op":"vpd","address":"0000:3b:00.0"}`+"\n"+`{"op":"dmi","name":"product_serial"}`+"\n")
	if err == nil || len(resps) != 1 || resps[0].Error != ErrCodeTimeout {
		t.Errorf("answers %+v, error %v; want one %s and an error", resps, err, ErrCodeTimeout)
	}
}

// A Handler built without a ReadTimeout falls back to DefaultReadTimeout
// rather than timing every read out at once.
func TestHandleDefaultsAZeroReadTimeout(t *testing.T) {
	resp, fatal := Handler{Root: hostRoot()}.Handle(Request{Op: OpDMI, Name: "product_serial"})
	if fatal || resp.Error != "" || string(resp.Data) != "EXSERIAL0001\n" {
		t.Errorf("zero ReadTimeout: fatal %v, %+v", fatal, resp)
	}
}
