package service_test

import (
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
	"github.com/compose-spec/compose-go/v2/types"
)

// Nothing here is brought up, mounted, or read: every case is a project value
// handed to the validator, which is a pure function over that value. No path
// named below is ever opened — /etc/shadow appears as a string because that is
// the string an attacker writes, not because the test touches it.

func appWithSecret(name, file string) *service.ComposeApp {
	p := mkApp("p", types.ServiceConfig{Name: "x", Image: "i", Secrets: []types.ServiceSecretConfig{{Source: name}}})
	p.Secrets = types.Secrets{name: types.SecretConfig{File: file}}
	return p
}

func appWithConfig(name, file string) *service.ComposeApp {
	p := mkApp("p", types.ServiceConfig{Name: "x", Image: "i", Configs: []types.ServiceConfigObjConfig{{Source: name}}})
	p.Configs = types.Configs{name: types.ConfigObjConfig{File: file}}
	return p
}

func appWithBindVolume(device string) *service.ComposeApp {
	p := mkApp("p", types.ServiceConfig{
		Name:    "x",
		Image:   "i",
		Volumes: []types.ServiceVolumeConfig{{Source: "appsdata", Target: "/mnt"}},
	})
	p.Volumes = types.Volumes{"appsdata": types.VolumeConfig{
		Driver:     "local",
		DriverOpts: map[string]string{"type": "none", "o": "bind", "device": device},
	}}
	return p
}

// ── F1: the file-backed secret/config channel ─────────────────────────

// A file-backed secret is a host bind wearing a different key. Docker mounts
// the named host file at /run/secrets/<name> inside the container, through the
// API path and through `docker compose -f … up -d` alike. The gate walked
// Services and Volumes and never Secrets, so the disguise it was built to stop
// went through unread three fields further along in the same document.
func TestComposeSecurity_StrictImport_BlocksHostFileSecret(t *testing.T) {
	err := service.ValidateComposeSecurity(appWithSecret("leak", "/etc/shadow"))
	if err == nil {
		t.Fatal("a secret reading a host file under /etc must be rejected: Docker mounts it into the container, which is arbitrary host file read")
	}
	if !strings.Contains(err.Error(), "/etc/shadow") {
		t.Fatalf("the refusal does not name the path it refused: %v", err)
	}
}

// Same channel, other field. A config with file: lands at its target.
func TestComposeSecurity_StrictImport_BlocksHostFileConfig(t *testing.T) {
	if err := service.ValidateComposeSecurity(appWithConfig("leak", "/etc/shadow")); err == nil {
		t.Fatal("a config reading a host file under /etc must be rejected")
	}
}

// The same denylist the bind rule uses, so the two cannot disagree: a secret
// pointing somewhere the bind policy allows is still allowed. If this ever
// needs to become "no host file at all", that is a policy decision, not a
// tightening to slip in here.
func TestComposeSecurity_AllowsASecretOutsideTheDenylist(t *testing.T) {
	if err := service.ValidateComposeSecurity(appWithSecret("ok", "/DATA/AppData/myapp/token")); err != nil {
		t.Fatalf("a secret under the app's own data tree must still be allowed: %v", err)
	}
}

// An inline secret has no host path at all and must not be confused for one.
func TestComposeSecurity_AllowsASecretWithNoFile(t *testing.T) {
	p := mkApp("p", types.ServiceConfig{Name: "x", Image: "i"})
	p.Secrets = types.Secrets{"inline": types.SecretConfig{Environment: "TOKEN"}}
	if err := service.ValidateComposeSecurity(p); err != nil {
		t.Fatalf("a secret with no file: must be allowed: %v", err)
	}
}

// The update gate has to see this too, or the install gate is the only lock on
// the door and ApplyComposeAppSettings walks around it.
func TestComposeSecurity_Update_BlocksNewlyAddedHostFileSecret(t *testing.T) {
	old := mkApp("p", types.ServiceConfig{Name: "x", Image: "i"})
	if err := service.ValidateComposeSecurityForUpdate(appWithSecret("leak", "/etc/shadow"), old); err == nil {
		t.Fatal("a host-file secret added on edit must be rejected")
	}
}

// ── F1b: the rule the update gate had lost ────────────────────────────

// THE ASYMMETRY. ValidateComposeSecurity rejected a top-level bind volume;
// ValidateComposeSecurityForUpdate never looked at project.Volumes at all. The
// reachable path is ApplyComposeAppSettings, which takes YAML from the request
// body and runs ONLY the update gate — so an account with PermAppConfigure
// could add exactly the volume that is refused at install.
//
// This is the tripwire: it goes red the moment the two gates stop sharing one
// rule set.
func TestComposeSecurity_Update_BlocksNewlyAddedNamedVolumeBind(t *testing.T) {
	old := mkApp("p", types.ServiceConfig{Name: "x", Image: "i"})
	err := service.ValidateComposeSecurityForUpdate(appWithBindVolume("/var/lib/casaos/apps"), old)
	if err == nil {
		t.Fatal("a named volume bind-mounting the apps tree, added on edit, must be rejected — the strict gate already rejects the identical volume at install")
	}
	if !strings.Contains(err.Error(), "newly added on edit") {
		t.Fatalf("the refusal does not read as an edit-time refusal: %v", err)
	}
}

// The update gate's whole purpose is that an app already carrying a dangerous
// setting keeps working through an unrelated edit. Extending the rule set must
// not break that, or the fix trades a hole for an outage.
func TestComposeSecurity_Update_KeepsAPreExistingNamedVolumeBind(t *testing.T) {
	old := appWithBindVolume("/var/lib/casaos/apps")
	newer := appWithBindVolume("/var/lib/casaos/apps")
	newer.Services["x"] = types.ServiceConfig{Name: "x", Image: "i:v2", Volumes: old.Services["x"].Volumes}
	if err := service.ValidateComposeSecurityForUpdate(newer, old); err != nil {
		t.Fatalf("a bind volume that was already installed must survive an image bump: %v", err)
	}
}

func TestComposeSecurity_Update_KeepsAPreExistingHostFileSecret(t *testing.T) {
	old := appWithSecret("leak", "/etc/shadow")
	if err := service.ValidateComposeSecurityForUpdate(appWithSecret("leak", "/etc/shadow"), old); err != nil {
		t.Fatalf("a secret that was already installed must survive an unrelated edit: %v", err)
	}
}

// Moving a pre-existing secret to a DIFFERENT host file is a new finding, not
// the old one: the Key carries the path precisely so the baseline cannot be
// used as a licence to read anything.
func TestComposeSecurity_Update_BlocksRepointingASecretAtAnotherHostFile(t *testing.T) {
	old := appWithSecret("leak", "/etc/shadow")
	if err := service.ValidateComposeSecurityForUpdate(appWithSecret("leak", "/etc/ssh/ssh_host_rsa_key"), old); err == nil {
		t.Fatal("repointing an existing secret at a different host file must be rejected")
	}
}

// A nil baseline still collapses to strict, as documented — now for the
// project-level rules too, not only the per-service ones that
// TestComposeSecurity_Update_NilBaselineIsStrict already pins.
func TestComposeSecurity_Update_NilBaselineIsStrictForVolumes(t *testing.T) {
	if err := service.ValidateComposeSecurityForUpdate(appWithBindVolume("/etc"), nil); err == nil {
		t.Fatal("with no baseline every finding is new; the bind volume must be rejected")
	}
}

// Ordinary projects must stay installable through both gates, or every
// assertion above is satisfiable by a validator that refuses everything.
func TestComposeSecurity_BothGatesAcceptAnOrdinaryProject(t *testing.T) {
	p := mkApp("p", types.ServiceConfig{
		Name:    "web",
		Image:   "nginx:latest",
		Volumes: []types.ServiceVolumeConfig{{Source: "data", Target: "/usr/share/nginx/html"}},
	})
	p.Volumes = types.Volumes{"data": types.VolumeConfig{Driver: "local"}}
	p.Secrets = types.Secrets{"tok": types.SecretConfig{File: "/DATA/AppData/web/token"}}

	if err := service.ValidateComposeSecurity(p); err != nil {
		t.Fatalf("strict gate rejected an ordinary project: %v", err)
	}
	if err := service.ValidateComposeSecurityForUpdate(p, nil); err != nil {
		t.Fatalf("update gate rejected an ordinary project: %v", err)
	}
}
