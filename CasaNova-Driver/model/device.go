package model

// PCIDevice is one entry under /sys/bus/pci/devices. Field shapes are modelled
// on the ZimaOS core GPU/device JSON (json:"vendor"/"product"/"businfo"/...)
// recovered from the oracle binary, generalised to all PCI classes.
type PCIDevice struct {
	Slot       string   `json:"slot"`                  // domain:bus:dev.func, e.g. 0000:01:00.0
	Class      string   `json:"class"`                 // 6-hex class code, e.g. 030000
	ClassName  string   `json:"class_name,omitempty"`  // human class, e.g. "VGA compatible controller"
	VendorID   string   `json:"vendor_id"`             // 4-hex, e.g. 10de
	VendorName string   `json:"vendor_name,omitempty"` // resolved from pci.ids when available
	DeviceID   string   `json:"device_id"`             // 4-hex
	DeviceName string   `json:"device_name,omitempty"` // resolved from pci.ids when available
	Revision   string   `json:"revision,omitempty"`
	Driver     string   `json:"driver"`            // bound kernel driver (module) name, "" if none
	Modules    []string `json:"modules,omitempty"` // candidate modules from modalias
}

// USBDevice is one real device node under /sys/bus/usb/devices (those exposing
// idVendor). Mirrors the CasaNova-LocalStorage USB model conventions.
type USBDevice struct {
	Node      string   `json:"node"`              // sysfs name, e.g. 1-1.2
	BusNum    string   `json:"bus_num"`           // busnum
	DevNum    string   `json:"dev_num"`           // devnum
	VendorID  string   `json:"vendor_id"`         // idVendor
	ProductID string   `json:"product_id"`        // idProduct
	Vendor    string   `json:"vendor,omitempty"`  // manufacturer string
	Product   string   `json:"product,omitempty"` // product string
	Serial    string   `json:"serial,omitempty"`  // serial string
	Class     string   `json:"class,omitempty"`   // bDeviceClass
	Speed     string   `json:"speed,omitempty"`   // speed in Mbps
	Drivers   []string `json:"drivers,omitempty"` // per-interface bound drivers
}

// KernelModule is one row of /proc/modules.
type KernelModule struct {
	Name      string   `json:"name"`
	Size      int64    `json:"size"`      // bytes
	RefCount  int      `json:"ref_count"` // usage count
	UsedBy    []string `json:"used_by"`   // dependent modules
	State     string   `json:"state"`     // Live | Loading | Unloading
	Address   string   `json:"address,omitempty"`
	OutOfTree bool     `json:"out_of_tree"` // true if under .../updates or extra (not in-tree)
}

// GPU is a display-class PCI device enriched with driver-binding info. NVIDIA
// entries are further enriched from nvidia-smi (external.NvidiaGPUInfoList).
type GPU struct {
	Slot          string `json:"slot"`
	Class         string `json:"class"`
	VendorID      string `json:"vendor_id"`
	Vendor        string `json:"vendor"` // NVIDIA | AMD | Intel | ...
	DeviceID      string `json:"device_id"`
	Product       string `json:"product,omitempty"`
	Driver        string `json:"driver"` // bound kernel driver
	DriverVersion string `json:"driver_version,omitempty"`
	UUID          string `json:"uuid,omitempty"`
	Serial        string `json:"serial,omitempty"`
	Branch        string `json:"branch,omitempty"` // active NVIDIA branch (open|legacy580|legacy470)
}
