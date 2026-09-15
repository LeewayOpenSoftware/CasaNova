package service_test

import (
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
	"github.com/compose-spec/compose-go/v2/types"
)

// Pure-value tests again: no container is created, inspected or named on a live
// daemon. "casanova-gateway" below is a string an attacker would write, not a
// container this test goes looking for.

// A service can leave its own namespace two ways. The gate knew one of them.
// `container:<name>` joins the namespace of a container this project does not
// own — another installed app, or one of CasaNova's own services, whose names
// are predictable. Sharing the network namespace of the gateway means reading
// its loopback traffic; sharing a PID namespace means seeing and signalling
// another app's processes.
func TestComposeSecurity_StrictImport_BlocksJoiningAnotherContainersNamespace(t *testing.T) {
	for _, tc := range []struct {
		field string
		svc   types.ServiceConfig
	}{
		{"network_mode", types.ServiceConfig{Name: "x", Image: "i", NetworkMode: "container:casanova-gateway"}},
		{"pid", types.ServiceConfig{Name: "x", Image: "i", Pid: "container:other-app"}},
		{"ipc", types.ServiceConfig{Name: "x", Image: "i", Ipc: "container:other-app"}},
	} {
		err := service.ValidateComposeSecurity(mkApp("p", tc.svc))
		if err == nil {
			t.Errorf("%s: container:<name> was accepted — the service joins a namespace it does not own", tc.field)
			continue
		}
		if !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: the refusal must name the field it refused; got %q", tc.field, err)
		}
	}
}

// Over-correction guards. These stay GREEN while the test above is red against
// the reverted behaviour, so "refuse everything that is not bridge" cannot pass
// for a fix.
func TestComposeSecurity_AllowsSameProjectAndOrdinaryNamespaceModes(t *testing.T) {
	// service:<name> points INSIDE this project. The app already owns every
	// namespace in it, so sharing one adds no reach. Refusing it would break the
	// standard sidecar shape (a VPN service others route through) for no gain.
	for _, svc := range []types.ServiceConfig{
		{Name: "x", Image: "i", NetworkMode: "service:vpn"},
		{Name: "x", Image: "i", Pid: "service:app"},
		{Name: "x", Image: "i", Ipc: "service:app"},
		{Name: "x", Image: "i", NetworkMode: "bridge"},
		{Name: "x", Image: "i", NetworkMode: "none"},
		{Name: "x", Image: "i", Ipc: "shareable"},
		{Name: "x", Image: "i", Ipc: "private"},
		// A user network literally called "containers" must not be mistaken for
		// the container: prefix, and the bare prefix names nothing.
		{Name: "x", Image: "i", NetworkMode: "containers"},
		{Name: "x", Image: "i", NetworkMode: "container:"},
	} {
		if err := service.ValidateComposeSecurity(mkApp("p", svc)); err != nil {
			t.Errorf("%+v must be allowed, got: %v", svc, err)
		}
	}
}

// host and container: are different privileges and must key differently, or an
// installed app that legitimately runs network_mode: host could be edited into
// container:casanova-gateway and the update gate would read it as the setting
// it already had.
func TestComposeSecurity_Update_HostCannotBeEditedIntoAContainerJoin(t *testing.T) {
	installed := mkApp("p", hostNetSvc("x"))
	joined := mkApp("p", types.ServiceConfig{Name: "x", Image: "img:latest", NetworkMode: "container:casanova-gateway"})

	if err := service.ValidateComposeSecurityForUpdate(joined, installed); err == nil {
		t.Fatal("network_mode: host was silently swapped for container:<name> on edit — different privilege, different key")
	}
	if err := service.ValidateComposeSecurityForUpdate(installed, joined); err == nil {
		t.Fatal("container:<name> was silently swapped for network_mode: host on edit")
	}
	// Re-pointing at a DIFFERENT container is a new privilege too.
	elsewhere := mkApp("p", types.ServiceConfig{Name: "x", Image: "img:latest", NetworkMode: "container:some-other"})
	if err := service.ValidateComposeSecurityForUpdate(elsewhere, joined); err == nil {
		t.Fatal("the join was re-pointed at another container on edit and passed")
	}
	// The same join, unchanged, still survives an image/tag edit.
	same := mkApp("p", types.ServiceConfig{Name: "x", Image: "img:next", NetworkMode: "container:casanova-gateway"})
	if err := service.ValidateComposeSecurityForUpdate(same, joined); err != nil {
		t.Fatalf("an unchanged pre-existing join must survive a tag change, got: %v", err)
	}
}
