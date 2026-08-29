package model

// Route describes a gateway proxy route.
// AllowedCIDRs, if non-empty, restricts which client IPs may access this route.
// Empty list = no restriction (default, backward compatible).
type Route struct {
	Path         string   `json:"path" binding:"required"`
	Target       string   `json:"target" binding:"required"`
	AllowedCIDRs []string `json:"allowed_cidrs,omitempty"` // e.g. ["192.168.1.0/24", "10.0.0.5/32"]
}

type ChangePortRequest struct {
	Port string `json:"port" binding:"required"`
}
