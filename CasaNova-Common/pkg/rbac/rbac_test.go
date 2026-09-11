package rbac

import (
	"encoding/json"
	"os"
	"testing"
)

// OLA 3 — specimen case. CasaNova-UI-v2/src/router/settingsAccess.ts's
// normalizeRole is a hand-written TypeScript mirror of Normalize below,
// verified to agree today, with nothing that FORCES them to keep agreeing:
// no shared code is possible across Go and TypeScript, so a future edit to
// either side (e.g. Go gaining a fourth role) has no mechanism to reach the
// other. This file and settingsAccess.spec.ts both read
// testdata/normalize_cases.json and assert the same cases against their own
// language's implementation — the fixture is the shared contract; a case
// added here without updating the TS side (or vice versa) is a visible,
// reviewed diff, not a silent drift.
//
// No rbac_test.go existed before this — Normalize had zero direct Go test
// coverage, so this is also the first assertion the canonical side actually
// behaves the way its own doc comment claims.
type normalizeCase struct {
	Input string `json:"input"`
	Want  string `json:"want"`
	Note  string `json:"note"`
}

func loadNormalizeCases(t *testing.T) []normalizeCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/normalize_cases.json")
	if err != nil {
		t.Fatalf("cannot read testdata/normalize_cases.json: %v", err)
	}
	var cases []normalizeCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("cannot parse testdata/normalize_cases.json: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("testdata/normalize_cases.json parsed to zero cases — anti-vacuity: " +
			"an empty fixture would make this test pass without checking anything")
	}
	return cases
}

func TestNormalizeMatchesSharedFixture(t *testing.T) {
	for _, tc := range loadNormalizeCases(t) {
		got := Normalize(tc.Input)
		if string(got) != tc.Want {
			t.Errorf("Normalize(%q) = %q, want %q (%s)", tc.Input, got, tc.Want, tc.Note)
		}
	}
}
