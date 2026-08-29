// Package pci provides a pure-Go (CGO-free) parser for the pci.ids database so
// PCI/USB vendor and device IDs can be resolved to human names without linking
// libpci. Parsing is best-effort: if the database is absent, IDs are returned
// unresolved and enumeration still works.
package pci

import (
	"bufio"
	"os"
	"strings"
	"sync"
)

// DB is a lazily-loaded pci.ids lookup table.
type DB struct {
	once    sync.Once
	path    string
	loaded  bool
	vendors map[string]string            // vendorID -> vendor name
	devices map[string]map[string]string // vendorID -> deviceID -> device name
}

// NewDB returns a DB backed by the pci.ids file at path (parsed on first use).
func NewDB(path string) *DB {
	return &DB{path: path}
}

func (d *DB) load() {
	d.once.Do(func() {
		d.vendors = make(map[string]string)
		d.devices = make(map[string]map[string]string)

		f, err := os.Open(d.path)
		if err != nil {
			return
		}
		defer f.Close()

		var curVendor string
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			// Class section starts with "C " — stop vendor parsing there.
			if strings.HasPrefix(line, "C ") {
				break
			}
			switch {
			case !strings.HasPrefix(line, "\t"):
				// "<vendorid>  Vendor Name"
				id, name, ok := splitIDName(line)
				if !ok {
					continue
				}
				curVendor = id
				d.vendors[id] = name
				d.devices[id] = make(map[string]string)
			case strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, "\t\t"):
				// "\t<deviceid>  Device Name"
				id, name, ok := splitIDName(strings.TrimPrefix(line, "\t"))
				if !ok || curVendor == "" {
					continue
				}
				d.devices[curVendor][id] = name
			}
			// "\t\t" subsystem lines are ignored.
		}
		d.loaded = true
	})
}

func splitIDName(s string) (id, name string, ok bool) {
	// Fields are separated by two spaces after the id.
	idx := strings.Index(s, "  ")
	if idx <= 0 {
		return "", "", false
	}
	id = strings.ToLower(strings.TrimSpace(s[:idx]))
	name = strings.TrimSpace(s[idx+2:])
	if id == "" || name == "" {
		return "", "", false
	}
	return id, name, true
}

// Vendor resolves a 4-hex vendor id (lower-case, no 0x). "" if unknown.
func (d *DB) Vendor(vendorID string) string {
	d.load()
	return d.vendors[strings.ToLower(vendorID)]
}

// Device resolves a device id within a vendor. "" if unknown.
func (d *DB) Device(vendorID, deviceID string) string {
	d.load()
	if m, ok := d.devices[strings.ToLower(vendorID)]; ok {
		return m[strings.ToLower(deviceID)]
	}
	return ""
}

// Available reports whether the pci.ids database was successfully parsed.
func (d *DB) Available() bool {
	d.load()
	return d.loaded && len(d.vendors) > 0
}
