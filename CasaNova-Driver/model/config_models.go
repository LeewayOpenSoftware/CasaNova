package model

// CommonModel maps the [common] section of driver.conf.
type CommonModel struct {
	RuntimePath string
}

// APPModel maps the [app] section of driver.conf.
type APPModel struct {
	LogPath     string
	LogSaveName string
	LogFileExt  string
}

// ServerModel maps the [server] section of driver.conf and carries the
// tunables specific to the driver service.
type ServerModel struct {
	// DriverDataRoot is the on-disk state root for out-of-tree driver packages.
	// Generic equivalent of the oracle's /opt/zimaos/drivers.
	DriverDataRoot string
	// DriverCLI is the optional package-manager CLI (install/enable/disable);
	// when present the driver service can shell out to it.
	DriverCLI string
	// GPUSelectBin is the GPU driver-branch selector invoked for GPU switching.
	GPUSelectBin string
	// PCIIDsPath is a pci.ids database used to resolve vendor/device names.
	PCIIDsPath string
}
