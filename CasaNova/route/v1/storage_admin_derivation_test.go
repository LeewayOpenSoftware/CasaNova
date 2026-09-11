package v1

import (
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
)

// OLA 3 — two independent derivations of storageauth.Principal.Admin used to
// exist: file.go's principalFrom used rbac.Allows(role, PermUserManage);
// maintenance_storage_engine.go's (*storageEngineAdapter).principal used a
// hand-rolled role == "admin" string compare. They agreed only because
// PermUserManage currently belongs to admin alone — a coincidence of the
// current bitmask, not a guarantee. Both now derive Admin the same way; this
// pins the invariant so a regression to the string-literal form fails here
// instead of silently reopening the gap the next time rbac.go's permission
// mapping changes.
func TestStorageEngineAdapterPrincipalAdminMatchesPermUserManage(t *testing.T) {
	a := &storageEngineAdapter{}
	for _, role := range rbac.AllRoles {
		want := rbac.Allows(role, rbac.PermUserManage)
		p := a.principal(StoragePrincipal{UserID: 1, Role: string(role)})
		if p.Admin != want {
			t.Errorf("role %s: principal().Admin = %v, want %v (rbac.Allows(role, PermUserManage))",
				role, p.Admin, want)
		}
	}
}
