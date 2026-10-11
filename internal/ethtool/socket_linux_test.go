//go:build linux

package ethtool

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ethtoolFeatureLine matches one feature line of `ethtool -k`, such as "rx-checksumming: on [fixed]" or "\ttx-checksum-ipv4: off [fixed]".
// Lines with other annotations ("off [requested on]") match on the name and value alone.
var ethtoolFeatureLine = regexp.MustCompile(`^\s*([^\s:]+): (on|off)( \[fixed\])?`)

func TestDialLoopbackFeatures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	lo, err := net.InterfaceByName("lo")
	if err != nil {
		t.Fatalf("InterfaceByName(lo): %v", err)
	}
	features, err := c.Features(uint32(lo.Index), lo.Name)
	if err != nil {
		t.Fatalf("Features(lo): %v", err)
	}
	if len(features) == 0 {
		t.Fatal("Features(lo) is empty")
	}

	t.Run("name does not match the index", func(t *testing.T) {
		if _, err := c.Features(uint32(lo.Index), "hwiapi-none"); !errors.Is(err, ErrNoDevice) {
			t.Errorf("Features(lo's index, another name) = %v, want ErrNoDevice", err)
		}
	})

	t.Run("matches ethtool -k", func(t *testing.T) {
		path, err := exec.LookPath("ethtool")
		if err != nil {
			t.Skip("ethtool is not on PATH")
		}
		out, err := exec.Command(path, "-k", "lo").Output()
		if err != nil {
			t.Fatalf("ethtool -k lo: %v", err)
		}
		compared := 0
		for _, line := range strings.Split(string(out), "\n") {
			m := ethtoolFeatureLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			f, ok := features[m[1]]
			if !ok {
				continue
			}
			compared++
			if active, fixed := m[2] == "on", m[3] != ""; f.Active != active || f.Fixed != fixed {
				t.Errorf("%s: netlink active %v fixed %v; ethtool -k says %q", m[1], f.Active, f.Fixed, line)
			}
		}
		if compared == 0 {
			t.Errorf("no feature of ethtool -k lo is in the netlink reply %v", features)
		}
	})
}
