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
