package route

import (
	"encoding/json"
	"net/http"
)

// InitDocRouter serves a minimal machine-readable route listing at V2DocPath.
// A full OpenAPI document is a ROADMAP item (see README).
func InitDocRouter() http.Handler {
	routes := map[string]any{
		"service": "casanova-driver",
		"paths": []string{
			V2APIPath + "/devices/pci",
			V2APIPath + "/devices/usb",
			V2APIPath + "/modules",
			V2APIPath + "/modules/{name}/load",
			V2APIPath + "/modules/{name}/unload",
			V2APIPath + "/gpu",
			V2APIPath + "/gpu/select",
			V2APIPath + "/drivers",
			V2APIPath + "/tuning/cpu-governor",
			V2APIPath + "/tuning/epp",
			V2APIPath + "/tuning/swappiness",
			V2APIPath + "/tuning/io-scheduler",
			V2APIPath + "/tuning/spindown",
			V2APIPath + "/tuning/sysctl",
			V2APIPath + "/tuning/zram",
			V2APIPath + "/sensors",
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != V2DocPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(routes)
	})
}
