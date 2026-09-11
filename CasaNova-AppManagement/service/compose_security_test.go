package service_test

import (
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
	"github.com/compose-spec/compose-go/v2/types"
)

func mkApp(name string, svcs ...types.ServiceConfig) *service.ComposeApp {
	m := types.Services{}
	for _, s := range svcs {
		m[s.Name] = s
	}
	p := service.ComposeApp(types.Project{Name: name, Services: m})
	return &p
}

func hostNetSvc(name string) types.ServiceConfig {
	return types.ServiceConfig{Name: name, Image: "img:latest", NetworkMode: "host"}
}

// ── New/import path stays strict ───────────────────────────────────────

func TestComposeSecurity_StrictImport_BlocksHostNetwork(t *testing.T) {
	if err := service.ValidateComposeSecurity(mkApp("cf", hostNetSvc("cf"))); err == nil {
		t.Fatal("strict import must reject network_mode: host")
	}
}

func TestComposeSecurity_StrictImport_BlocksDangerousBindAndPrivileged(t *testing.T) {
	bind := types.ServiceConfig{Name: "x", Image: "i", Volumes: []types.ServiceVolumeConfig{{Type: "bind", Source: "/etc/passwd", Target: "/etc/passwd"}}}
	if err := service.ValidateComposeSecurity(mkApp("x", bind)); err == nil {
		t.Fatal("strict import must reject a dangerous host bind mount")
	}
	if err := service.ValidateComposeSecurity(mkApp("p", types.ServiceConfig{Name: "p", Image: "i", Privileged: true})); err == nil {
		t.Fatal("strict import must reject privileged: true")
	}
}

// ── Existing-app edit/update grandfathers unchanged settings ───────────

func TestComposeSecurity_Update_PreservesExistingHostNetwork(t *testing.T) {
	// Cloudflared case: the installed app already uses network_mode: host;
	// a tag/image change must NOT be re-blocked.
	old := mkApp("cf", types.ServiceConfig{Name: "cf", Image: "wisdomsky/cloudflared-web:1.0", NetworkMode: "host"})
	updated := mkApp("cf", types.ServiceConfig{Name: "cf", Image: "wisdomsky/cloudflared-web:latest", NetworkMode: "host"})
	if err := service.ValidateComposeSecurityForUpdate(updated, old); err != nil {
		t.Fatalf("unchanged network_mode: host must be allowed on update, got: %v", err)
	}
}

func TestComposeSecurity_Update_BlocksNewlyAddedHostNetwork(t *testing.T) {
	old := mkApp("cf", types.ServiceConfig{Name: "cf", Image: "i", NetworkMode: "bridge"})
	updated := mkApp("cf", hostNetSvc("cf"))
	if err := service.ValidateComposeSecurityForUpdate(updated, old); err == nil {
		t.Fatal("newly added network_mode: host must be rejected on update")
	}
}

func TestComposeSecurity_Update_PreservesExistingBind_BlocksNewBind(t *testing.T) {
	withBinds := func(name string, srcs ...string) types.ServiceConfig {
		s := types.ServiceConfig{Name: name, Image: "i"}
		for _, src := range srcs {
			s.Volumes = append(s.Volumes, types.ServiceVolumeConfig{Type: "bind", Source: src, Target: src})
		}
		return s
	}
	old := mkApp("a", withBinds("a", "/etc/foo"))
	if err := service.ValidateComposeSecurityForUpdate(mkApp("a", withBinds("a", "/etc/foo")), old); err != nil {
		t.Fatalf("keeping an existing dangerous bind must be allowed, got: %v", err)
	}
	if err := service.ValidateComposeSecurityForUpdate(mkApp("a", withBinds("a", "/etc/foo", "/usr/bar")), old); err == nil {
		t.Fatal("a newly added dangerous bind mount must be rejected on update")
	}
}

func TestComposeSecurity_Update_PreservesExistingPrivileged_BlocksNew(t *testing.T) {
	old := mkApp("p", types.ServiceConfig{Name: "p", Image: "i", Privileged: true})
	if err := service.ValidateComposeSecurityForUpdate(mkApp("p", types.ServiceConfig{Name: "p", Image: "i2", Privileged: true}), old); err != nil {
		t.Fatalf("unchanged privileged must be allowed on update, got: %v", err)
	}
	oldNoPriv := mkApp("q", types.ServiceConfig{Name: "q", Image: "i"})
	if err := service.ValidateComposeSecurityForUpdate(mkApp("q", types.ServiceConfig{Name: "q", Image: "i", Privileged: true}), oldNoPriv); err == nil {
		t.Fatal("newly added privileged must be rejected on update")
	}
}

func TestComposeSecurity_Update_NilBaselineIsStrict(t *testing.T) {
	if err := service.ValidateComposeSecurityForUpdate(mkApp("cf", hostNetSvc("cf")), nil); err == nil {
		t.Fatal("a nil baseline must behave strictly (reject host network)")
	}
}

// ── AppsPath stays unbindable ──────────────────────────────────────────
//
// These pin a precondition that something ELSE depends on. ComposeApp.Uninstall
// deletes a path derived from a sideloaded compose file's volume.Source and
// confines it with a LEXICAL prefix check against config.AppInfo.AppsPath
// (/var/lib/casaos/apps). That check resolves nothing, so a symlink among the
// components under AppsPath would be followed by os.RemoveAll and delete
// outside the tree.
//
// It is unreachable today only because planting that symlink needs write access
// to /var/lib/casaos/apps, which needs host root already: the file API refuses
// /var outright (utils.BlockedSystemPrefixes), and the checks below stop a
// compose file from bind-mounting the tree into a container to do it from
// inside. Relaxing dangerousBindSources would make that delete path live, so
// the coverage belongs here rather than only in the audit that found it.
func TestComposeSecurity_BlocksBindingTheAppsTree(t *testing.T) {
	for _, src := range []string{
		"/var/lib/casaos/apps",                  // AppsPath itself
		"/var/lib/casaos/apps/navidrome/config", // a child of it
		"/var/lib/casaos",                       // its parent
		"/var/lib",                              // an ancestor
		"/var/lib/casaos/apps/",                 // trailing separator
		"/var/lib/casaos/../casaos/apps",        // un-cleaned equivalent
	} {
		svc := types.ServiceConfig{
			Name:    "x",
			Image:   "i",
			Volumes: []types.ServiceVolumeConfig{{Type: "bind", Source: src, Target: "/mnt"}},
		}
		if err := service.ValidateComposeSecurity(mkApp("p", svc)); err == nil {
			t.Fatalf("bind of %q must be rejected: it would let a container plant a symlink under AppsPath", src)
		}
	}
}

// Same tree, reached through a project-level named volume whose local driver
// carries driver_opts device=<path> — a host bind in disguise, which the
// per-service bind check skips because the service's source is a bare name.
func TestComposeSecurity_BlocksNamedVolumeBindOfAppsTree(t *testing.T) {
	p := mkApp("p", types.ServiceConfig{
		Name:    "x",
		Image:   "i",
		Volumes: []types.ServiceVolumeConfig{{Source: "appsdata", Target: "/mnt"}},
	})
	p.Volumes = types.Volumes{"appsdata": types.VolumeConfig{
		Driver: "local",
		DriverOpts: map[string]string{
			"type":   "none",
			"o":      "bind",
			"device": "/var/lib/casaos/apps",
		},
	}}
	if err := service.ValidateComposeSecurity(p); err == nil {
		t.Fatal("named volume bind-mounting AppsPath must be rejected")
	}
}
