package v2

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// delete_config_folder promises more than it does, and the promise is the part
// that is wrong.
//
// What an uninstall removes REGARDLESS of the flag: the containers, the images,
// the app's named Docker volumes, and its working directory under AppsPath.
// What the flag actually governs: one extra sweep over each service's volume
// sources, confined to AppsPath by a strict lexical prefix check. An ordinary
// app's sources are under the DATA area, not AppsPath, so the sweep matches
// nothing and the flag changes nothing. The user's data survives either way.
//
// The behaviour is deliberate and stays — repointing that sweep would turn a
// silent no-op into a recursive delete of user data, which is not a change that
// travels in a commit like this one. What was never updated is the contract:
// "Delete the config folder of the app", default true, is what an API consumer
// reads and believes. These tests pin the corrected wording so a regeneration,
// or a well-meaning revert, cannot quietly restore the claim.

const (
	openAPIPath = "../../api/app_management/openapi.yaml"
	codegenPath = "../../codegen/app_management_api.go"
)

func readContractFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	// Anti-vacuity: an empty or truncated read would satisfy every "does not
	// contain" assertion below without proving anything.
	if len(b) < 2000 {
		t.Fatalf("%s came back %d bytes — that is not the file, fix the path", path, len(b))
	}
	return string(b)
}

// yamlDescription returns the `description:` line belonging to a named
// parameter block in the OpenAPI document.
func yamlDescription(t *testing.T, doc, param string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^    ` + regexp.QuoteMeta(param) + `:\r?\n(?:.*\r?\n)*?      description: (.*)`)
	m := re.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("no description found for parameter %q — the document shape changed", param)
	}
	return strings.TrimSpace(m[1])
}

func TestDeleteConfigFolderContractDoesNotPromiseADataWipe(t *testing.T) {
	desc := yamlDescription(t, readContractFile(t, openAPIPath), "DeleteConfigFolder")

	// The old wording, and the reason this test exists.
	if strings.EqualFold(desc, "Delete the config folder of the app") {
		t.Fatal("the parameter is advertised as deleting the app's config folder again; " +
			"for an ordinary app it deletes nothing, and a caller who believes this " +
			"will think their data was removed when it was not")
	}
	// The two facts a caller has to be told, because both are surprising.
	for _, want := range []string{"apps directory", "keeps its data"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description no longer states %q; it reads: %s", want, desc)
		}
	}
}

// The generated comment is derived from the description verbatim —
// "// <FieldName> <description>" — so the two drift apart only if someone edits
// one of them by hand or regenerates against a stale document. Either way the
// contract and the client stop agreeing about what the parameter does.
func TestGeneratedCommentMatchesTheContract(t *testing.T) {
	doc := readContractFile(t, openAPIPath)
	code := readContractFile(t, codegenPath)

	for _, tc := range []struct{ param, field string }{
		{"DeleteConfigFolder", "DeleteConfigFolder"},
		// POSITIVE CONTROL: an untouched parameter, proving the extraction and
		// the comparison actually work. Without it, a yamlDescription that
		// silently returned the wrong line would make the case above pass.
		{"PullLatestImage", "Pull"},
	} {
		desc := yamlDescription(t, doc, tc.param)
		want := "// " + tc.field + " " + desc
		if !strings.Contains(code, want) {
			t.Errorf("codegen comment for %s does not match the OpenAPI description.\n want: %s",
				tc.param, want)
		}
	}
}
