package service

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Driver/model"
	"github.com/LeewayOpenSoftware/CasaNova-Driver/pkg/pci"
)

const (
	sysfsPCIDevices = "/sys/bus/pci/devices"
	sysfsUSBDevices = "/sys/bus/usb/devices"
)

// DeviceService enumerates PCI and USB hardware straight from sysfs (no CGO).
type DeviceService interface {
	ListPCI() ([]model.PCIDevice, error)
	ListUSB() ([]model.USBDevice, error)
}

type deviceService struct {
	ids *pci.DB
}

func NewDeviceService(ids *pci.DB) DeviceService {
	return &deviceService{ids: ids}
}

// pciClassNames maps the high byte of the PCI class code to a human label.
var pciClassNames = map[string]string{
	"00": "Unclassified device",
	"01": "Mass storage controller",
	"02": "Network controller",
	"03": "Display controller",
	"04": "Multimedia controller",
	"05": "Memory controller",
	"06": "Bridge",
	"07": "Communication controller",
	"08": "Generic system peripheral",
	"09": "Input device controller",
	"0a": "Docking station",
	"0b": "Processor",
	"0c": "Serial bus controller",
	"0d": "Wireless controller",
	"0e": "Intelligent controller",
	"0f": "Satellite communications controller",
	"10": "Encryption controller",
	"11": "Signal processing controller",
	"12": "Processing accelerators",
	"13": "Non-Essential Instrumentation",
}

func readSysAttr(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// trimHex strips a leading 0x and lowercases a sysfs hex attribute.
func trimHex(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "0x"))
}

func (d *deviceService) ListPCI() ([]model.PCIDevice, error) {
	entries, err := os.ReadDir(sysfsPCIDevices)
	if err != nil {
		return nil, err
	}

	devices := make([]model.PCIDevice, 0, len(entries))
	for _, e := range entries {
		slot := e.Name() // already domain:bus:dev.func
		dir := filepath.Join(sysfsPCIDevices, slot)

		class := trimHex(readSysAttr(dir, "class")) // e.g. 030000
		vendor := trimHex(readSysAttr(dir, "vendor"))
		device := trimHex(readSysAttr(dir, "device"))

		dev := model.PCIDevice{
			Slot:      slot,
			Class:     class,
			VendorID:  vendor,
			DeviceID:  device,
			Revision:  trimHex(readSysAttr(dir, "revision")),
			ClassName: className(class),
			Driver:    resolveDriver(dir),
			Modules:   modaliasModules(dir),
		}
		if d.ids != nil {
			dev.VendorName = d.ids.Vendor(vendor)
			dev.DeviceName = d.ids.Device(vendor, device)
		}
		devices = append(devices, dev)
	}

	sort.Slice(devices, func(i, j int) bool { return devices[i].Slot < devices[j].Slot })
	return devices, nil
}

func className(class string) string {
	if len(class) >= 2 {
		if n, ok := pciClassNames[class[:2]]; ok {
			return n
		}
	}
	return ""
}

// resolveDriver returns the bound kernel driver by reading the "driver" symlink.
func resolveDriver(dir string) string {
	target, err := os.Readlink(filepath.Join(dir, "driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// modaliasModules is a placeholder for candidate-module resolution. Full
// resolution needs modules.alias matching; for now we surface nothing rather
// than guess. See ROADMAP in README.
func modaliasModules(dir string) []string {
	return nil
}

func (d *deviceService) ListUSB() ([]model.USBDevice, error) {
	entries, err := os.ReadDir(sysfsUSBDevices)
	if err != nil {
		return nil, err
	}

	devices := make([]model.USBDevice, 0)
	for _, e := range entries {
		node := e.Name()
		// Interfaces look like "1-1:1.0"; skip them, we only want device nodes.
		if strings.Contains(node, ":") {
			continue
		}
		dir := filepath.Join(sysfsUSBDevices, node)
		idVendor := trimHex(readSysAttr(dir, "idVendor"))
		if idVendor == "" {
			// Root hubs / buses without idVendor are skipped.
			continue
		}
		dev := model.USBDevice{
			Node:      node,
			BusNum:    readSysAttr(dir, "busnum"),
			DevNum:    readSysAttr(dir, "devnum"),
			VendorID:  idVendor,
			ProductID: trimHex(readSysAttr(dir, "idProduct")),
			Vendor:    readSysAttr(dir, "manufacturer"),
			Product:   readSysAttr(dir, "product"),
			Serial:    readSysAttr(dir, "serial"),
			Class:     trimHex(readSysAttr(dir, "bDeviceClass")),
			Speed:     readSysAttr(dir, "speed"),
			Drivers:   usbInterfaceDrivers(dir, node),
		}
		devices = append(devices, dev)
	}

	sort.Slice(devices, func(i, j int) bool { return devices[i].Node < devices[j].Node })
	return devices, nil
}

// usbInterfaceDrivers collects the bound driver of each interface of a device
// (interfaces are sub-directories named "<node>:<config>.<intf>").
func usbInterfaceDrivers(dir, node string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var drivers []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, node+":") {
			continue
		}
		if drv := resolveDriver(filepath.Join(dir, name)); drv != "" && !seen[drv] {
			seen[drv] = true
			drivers = append(drivers, drv)
		}
	}
	sort.Strings(drivers)
	return drivers
}
