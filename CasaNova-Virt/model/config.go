package model

// CommonModel mirrors CasaNova-LocalStorage/model.CommonModel: the [common]
// section of the .conf file.
type CommonModel struct {
	RuntimePath string
}

// APPModel mirrors the [app] section of the .conf file.
type APPModel struct {
	LogPath     string
	LogSaveName string
	LogFileExt  string
}

// ServerModel holds the [server] section: virt-specific tunables.
type ServerModel struct {
	// LibvirtSocket is the path to the libvirt RPC unix socket. ZimaOS runs
	// the modular libvirt daemons, so the default points at virtqemud's
	// socket; the monolithic libvirtd socket is the documented fallback.
	LibvirtSocket string
	// ImageDir is where downloaded ISOs / OS images land (oracle used
	// /DATA/Downloads/ISO).
	ImageDir string
	// DiskDir is the qcow2 backing-store directory (libvirt default pool).
	DiskDir string
}
