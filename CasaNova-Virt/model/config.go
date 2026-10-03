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
	// LibvirtSocket is the libvirt RPC unix socket to try first. When it does
	// not exist, libvirtconn.ResolveSocket falls back to virtqemud-sock
	// (modular daemons, ZimaOS) and then libvirt-sock (monolithic libvirtd,
	// stock Ubuntu/Debian).
	LibvirtSocket string
	// ImageDir is where downloaded ISOs / OS images land (oracle used
	// /DATA/Downloads/ISO).
	ImageDir string
	// DiskDir is the qcow2 backing-store directory (libvirt default pool).
	DiskDir string
}
