package v1

import (
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-UserService/service"
	dbmodel "github.com/LeewayOpenSoftware/CasaNova-UserService/service/model"
)

// fakeUserServiceRoster backs countOtherAdmins's bulk-fetch path.
type fakeUserServiceRoster struct {
	service.UserService
	rows []dbmodel.UserDBModel
}

func (f fakeUserServiceRoster) GetAllUserName() []dbmodel.UserDBModel { return f.rows }

type fakeRepoRoster struct {
	service.Repository
	user fakeUserServiceRoster
}

func (f fakeRepoRoster) User() service.UserService { return f.user }

func withRoster(t *testing.T, rows []dbmodel.UserDBModel) {
	t.Helper()
	prev := service.MyService
	service.MyService = fakeRepoRoster{user: fakeUserServiceRoster{rows: rows}}
	t.Cleanup(func() { service.MyService = prev })
}

// This is the row shape god's correction targeted: a legacy account whose
// Role column literally says "administrator" — never "admin" — but resolves
// to admin at token-mint time via resolveEffectiveRole (Wave 35-U / Role
// Mint Parity). Counting by the literal string misses this account entirely.
func TestCountOtherAdminsCountsLegacyRoleStrings(t *testing.T) {
	withRoster(t, []dbmodel.UserDBModel{
		{Id: 1, Role: "administrator"}, // legacy row, resolves to admin
		{Id: 2, Role: "user"},
	})
	if n := countOtherAdmins("999"); n != 1 {
		t.Fatalf("countOtherAdmins = %d, want 1 — a row stored as \"administrator\" "+
			"resolves to admin and must be counted", n)
	}
}

// The mirror failure mode: two admins where one is a legacy row must count
// as TWO, not one — otherwise a legitimate delete/demote gets refused.
func TestCountOtherAdminsDoesNotCollapseTwoDistinctAdmins(t *testing.T) {
	withRoster(t, []dbmodel.UserDBModel{
		{Id: 1, Role: "admin"},
		{Id: 2, Role: "administrator"},
	})
	if n := countOtherAdmins("999"); n != 2 {
		t.Fatalf("countOtherAdmins = %d, want 2 — an \"admin\" row and an \"administrator\" "+
			"row are two distinct admin accounts, not one", n)
	}
}

func TestCountOtherAdminsExcludesTheGivenID(t *testing.T) {
	withRoster(t, []dbmodel.UserDBModel{
		{Id: 1, Role: "admin"},
		{Id: 2, Role: "user"},
	})
	if n := countOtherAdmins("1"); n != 0 {
		t.Fatalf("countOtherAdmins(exclude=1) = %d, want 0", n)
	}
}

// Fail-closed by construction: an empty/unreachable roster counts as zero
// other admins, which is the conservative (refuse-the-operation) outcome.
func TestCountOtherAdminsFailsClosedOnEmptyRoster(t *testing.T) {
	withRoster(t, nil)
	if n := countOtherAdmins("1"); n != 0 {
		t.Fatalf("countOtherAdmins = %d, want 0 on an empty/failed fetch", n)
	}
}
