package model

// InstalledDriver mirrors one row of the oracle's `zimaos-driver list` table
// (NAME / STATE / RUNTIME / PAYLOAD / MODULES), i.e. an out-of-tree driver
// package installed and managed on-disk. See ANALYSIS.md for the state layout.
type InstalledDriver struct {
	Name    string   `json:"name"`
	State   string   `json:"state"`   // enabled | disabled
	Runtime string   `json:"runtime"` // loaded | partial | inactive | residual | firmware | disabled
	Payload string   `json:"payload"` // module | firmware | module+firmware
	Modules []string `json:"modules"` // configured module names
}
