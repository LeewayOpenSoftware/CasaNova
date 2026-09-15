package service_test

import (
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/service"
	"github.com/compose-spec/compose-go/v2/types"
)

// Nothing here is started, mounted or read. Every case is a plain struct handed
// to the validator, which is a pure function over that value.

func cgroupSvc(mode string) types.ServiceConfig {
	return types.ServiceConfig{Name: "x", Image: "i", Cgroup: mode}
}

// `cgroup: host` is not `cgroup_parent:`. compose-go declares the two on
// adjacent lines and the gate read only the second, so the sixth host namespace
// went through while the other five were denied.
func TestComposeSecurity_StrictImport_BlocksHostCgroupNamespace(t *testing.T) {
	err := service.ValidateComposeSecurity(mkApp("p", cgroupSvc("host")))
	if err == nil {
		t.Fatal("cgroup: host was accepted — the container joins the host cgroup namespace")
	}
	if !strings.Contains(err.Error(), "cgroup: host") {
		t.Fatalf("the refusal must name the field and value it refused; got %q", err)
	}
}

// Over-correction guard: it must still be the VALUE that is refused, not the
// field. `cgroup: private` is the compose default and the safe setting; a gate
// that refused it would satisfy the test above while breaking every app that
// spells the default out.
func TestComposeSecurity_AllowsPrivateAndUnsetCgroup(t *testing.T) {
	for _, mode := range []string{"", "private"} {
		if err := service.ValidateComposeSecurity(mkApp("p", cgroupSvc(mode))); err != nil {
			t.Errorf("cgroup: %q must be allowed (it is the default), got: %v", mode, err)
		}
	}
}

// cgroup_parent is an arbitrary parent cgroup name/path, not the cgroup
// namespace selector above. In particular, "host" has no reserved meaning in
// this field and must remain usable as a legitimate parent name.
func TestComposeSecurity_AllowsHostNamedCgroupParent(t *testing.T) {
	svc := types.ServiceConfig{Name: "x", Image: "i", CgroupParent: "host"}
	if err := service.ValidateComposeSecurity(mkApp("p", svc)); err != nil {
		t.Fatalf("cgroup_parent: host is a legitimate parent name and must be allowed, got: %v", err)
	}
}

// The update gate takes its rules from the same collection function, so the new
// check reaches it too: pre-existing stays, newly added does not.
func TestComposeSecurity_Update_CgroupHostFollowsTheBaselineRule(t *testing.T) {
	updated := mkApp("p", cgroupSvc("host"))

	if err := service.ValidateComposeSecurityForUpdate(updated, mkApp("p", cgroupSvc("host"))); err != nil {
		t.Fatalf("an already-installed cgroup: host must survive an edit, got: %v", err)
	}
	if err := service.ValidateComposeSecurityForUpdate(updated, mkApp("p", cgroupSvc("private"))); err == nil {
		t.Fatal("cgroup: host added during an edit must be rejected")
	}
}
