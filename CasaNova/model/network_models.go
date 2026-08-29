package model

// Network and sharing config models. Consolidated from ddns.go and share.go.

// GoDaddyModel holds dynamic DNS configuration for the GoDaddy provider.
type GoDaddyModel struct {
	Type    uint   `json:"type"`
	ApiHost string `json:"api_host"`
	Key     string `json:"-"` // Never serialize — API credentials
	Secret  string `json:"-"` // Never serialize — API credentials
	Host    string `json:"host"`
}

// Shares is an SMB/NFS shared folder definition.
// Anonymous controls whether guest access is permitted on the share.
type Shares struct {
	ID        uint   `json:"id"`
	Anonymous bool   `json:"anonymous"`
	Path      string `json:"path"`
}
