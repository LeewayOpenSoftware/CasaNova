package service

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The sysfs listings replaced a bash pipeline whose exact output shape the
// hardware broadcast depends on, so pin the order, the virtual-interface
// filtering and the trailing empty element the pipeline used to produce.
func fakeSysNet(t *testing.T, class, virtual []string) {
	t.Helper()

	root := t.TempDir()
	classDir := filepath.Join(root, "class-net")
	virtualDir := filepath.Join(root, "virtual-net")
	for _, name := range class {
		if err := os.MkdirAll(filepath.Join(classDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range virtual {
		if err := os.MkdirAll(filepath.Join(virtualDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	oldClass, oldVirtual := sysClassNetPath, sysVirtualNetPath
	sysClassNetPath, sysVirtualNetPath = classDir, virtualDir
	t.Cleanup(func() { sysClassNetPath, sysVirtualNetPath = oldClass, oldVirtual })
}

func TestGetNetSplitsPhysicalFromVirtual(t *testing.T) {
	fakeSysNet(t,
		[]string{"wlan0", "eth0", "docker0", "lo", "br-a1b2", "veth9f0"},
		[]string{"docker0", "lo", "br-a1b2", "veth9f0"},
	)

	c := &systemService{}

	if got, want := c.GetNet(true), []string{"eth0", "wlan0", ""}; !reflect.DeepEqual(got, want) {
		t.Errorf("GetNet(true) = %q, want %q", got, want)
	}
	if got, want := c.GetNet(false), []string{"br-a1b2", "docker0", "lo", "veth9f0", ""}; !reflect.DeepEqual(got, want) {
		t.Errorf("GetNet(false) = %q, want %q", got, want)
	}
}

func TestGetNetState(t *testing.T) {
	fakeSysNet(t, []string{"eth0"}, []string{"lo"})

	if err := os.WriteFile(filepath.Join(sysClassNetPath, "eth0", "operstate"), []byte("up\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &systemService{}

	// The callers trim it, so the trailing newline sysfs writes must survive.
	if got := c.GetNetState("eth0"); got != "up\n" {
		t.Errorf("GetNetState(eth0) = %q, want %q", got, "up\n")
	}
	if got := c.GetNetState("nope0"); got != "" {
		t.Errorf("GetNetState(nope0) = %q, want empty", got)
	}
	// A name carrying a separator must never be pasted into the sysfs path.
	if got := c.GetNetState("../../../etc/hostname"); got != "" {
		t.Errorf("GetNetState(traversal) = %q, want empty", got)
	}
}
