package v1

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// THE PRODUCTION WIRING CENSUS.
//
// This codebase's signature defect is a correct security mechanism that no
// production caller reaches. It has happened seven times: a systemd unit with
// no [Install] section, a oneshot no script enabled, two scripts the Makefile
// never packaged, 350 lines of bounding logic with `_ = ctx` beside a direct
// syscall, an authorization gate that wrote 403 and continued, a revocation
// hook declared and never assigned, and — most recently — a storage authority
// with 27 tests, eight killed mutations and zero callers.
//
// So the storage authority does not get to be trusted because it exists. This
// test walks the production tree, finds every place a user-supplied path is
// sanitised, and requires that the same function also consults the authority.
// A handler added next year that validates a path the old way fails here, by
// file and line, on the day it is written.
//
// It is not a lint rule about style. Sanitising a path is the marker of "this
// function is about to act on a location a caller chose"; the census asserts
// that every such function asks who is allowed to.

// sanitiserCall marks a function as handling a caller-supplied path.
var sanitiserCall = regexp.MustCompile(`utils\.SanitizeFilePath(NoFollow)?\(`)

// authorityCall marks the function as consulting the storage authority, either
// directly or through this package's chokepoint helpers.
var authorityCall = regexp.MustCompile(`storageauth\.|authorizePath|validatePath`)

// chokepoints are the ONLY functions permitted to call the sanitiser directly.
//
// After wiring, a direct utils.SanitizeFilePath* call is itself the signal: it
// means a function decided for itself what a caller may touch. The legitimate
// direct callers are the few places that then hand the result to the authority.
//
// Granularity matters here, and the first version of this census got it wrong.
// It asked "does the enclosing FUNCTION mention the authority somewhere", which
// a large handler satisfies trivially — DeleteFile authorizes one path and can
// still sanitise another the old way three lines later. Proven: deliberately
// unwiring DeleteFile's permanent-delete branch left this test green. Per call
// site is the only granularity that catches it.
var chokepoints = map[string]string{
	"authorizePathWith":  "route/v1: the single decision point every file endpoint goes through",
	"sanitizeUploadPath": "service: the upload path's own chokepoint, which then calls AuthorizeSystem",
	"getTrashManager":    "route/v1: installs the restore DestValidator, which then calls AuthorizeSystem",
	"PostUploadFile":     "route/v2: sanitises then immediately calls AuthorizeStorage with the request's principal",
	"serveV3File":        "route: GET /v3/file sanitises then opens through OpenAuthorizedStrict with the token's principal",
}

// exemptedFiles sanitise paths WITHOUT the storage authority for a recorded
// reason. An exemption is a deliberate, reviewable decision — never a silent
// skip.
var exemptedFiles = map[string]string{
	"pkg/maintenance/guard.go": "the maintenance module operates on system locations by design " +
		"(rotated logs, orphaned temp files, package caches) and carries its own root model. " +
		"Those paths are deliberately OUTSIDE the storage the file manager serves, so the " +
		"storage ceiling would refuse all of them. Reviewed and kept separate.",
}

func TestEveryPathHandlingFunctionConsultsTheStorageAuthority(t *testing.T) {
	root := moduleRoot(t)

	var unwired []string
	scanned := 0

	for _, dir := range []string{"route", "service", "pkg"} {
		err := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)

			if !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") ||
				strings.Contains(rel, "pkg/storageauth/") ||
				strings.Contains(rel, "pkg/utils/") {
				return nil
			}
			if _, ok := exemptedFiles[rel]; ok {
				return nil
			}

			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			src := string(data)

			for _, m := range sanitiserCall.FindAllStringIndex(src, -1) {
				scanned++
				name, _ := enclosingFunc(src, m[0])
				if _, ok := chokepoints[name]; ok {
					continue
				}
				line := 1 + strings.Count(src[:m[0]], "\n")
				unwired = append(unwired, rel+":"+strconv.Itoa(line)+"  func "+name)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	if scanned == 0 {
		t.Skip("no path-handling call sites found; not running against the full module")
	}
	if len(unwired) > 0 {
		t.Fatalf("%d call site(s) sanitise a caller-supplied path WITHOUT going through the "+
			"storage authority:\n  %s\n\nEach one decides for itself what a caller may touch, "+
			"which is the model LEG-001 replaced. Wire it through authorizePath (request paths, "+
			"which have a principal) or storageauth.AuthorizeSystem (startup, recovery, cleanup "+
			"and rehydration, which do not). A new chokepoint goes in `chokepoints`; a genuine "+
			"exception goes in `exemptedFiles`, with its reason.",
			len(unwired), strings.Join(unwired, "\n  "))
	}
	t.Logf("all %d sanitiser call sites are inside a chokepoint (%d chokepoint(s), %d exempted file(s))",
		scanned, len(chokepoints), len(exemptedFiles))
}

// The startup, recovery and cleanup paths are the ones a previous vulnerable
// release can poison, so they are censused explicitly rather than being assumed
// to be covered by the HTTP sweep above.
func TestNonRequestPathsConsultTheAuthority(t *testing.T) {
	root := moduleRoot(t)

	// file -> the function that must consult the authority.
	required := map[string]string{
		"route/v1/trash.go":         "DestValidator", // restore, from untrusted .trashinfo
		"service/file_upload.go":    "sanitizeUploadPath",
		"route/v2/file.go":          "PostUploadFile",
		"pkg/storageauth/policy.go": "evaluateStorage", // the authority itself must exist
	}

	for rel, fn := range required {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		src := string(data)
		i := strings.Index(src, fn)
		if i < 0 {
			t.Fatalf("%s no longer contains %s; the census is out of date and must be updated "+
				"deliberately, not silently", rel, fn)
		}
		_, body := enclosingFunc(src, i)
		if rel != "pkg/storageauth/policy.go" && !authorityCall.MatchString(body) {
			t.Fatalf("%s: %s acts on a path without consulting the storage authority. "+
				"This is a non-request path — startup, recovery, cleanup or rehydration — "+
				"which is exactly where a vulnerable predecessor's persisted state gets "+
				"executed by the fixed build.", rel, fn)
		}
	}
}

// enclosingFunc returns the name and body of the function containing offset i.
func enclosingFunc(src string, i int) (string, string) {
	j := strings.LastIndex(src[:i], "\nfunc ")
	if j < 0 {
		return "<file scope>", src
	}
	// Handles both "func Name(" and "func (r *Recv) Name(" — a method whose
	// name was reported as "?" is a call site nobody can act on.
	decl := src[j+6:]
	if nl := strings.IndexByte(decl, '\n'); nl > 0 {
		decl = decl[:nl]
	}
	name := "?"
	if strings.HasPrefix(decl, "(") {
		if close := strings.IndexByte(decl, ')'); close > 0 {
			rest := strings.TrimSpace(decl[close+1:])
			if open := strings.IndexByte(rest, '('); open > 0 {
				name = strings.TrimSpace(rest[:open])
			}
		}
	} else if open := strings.IndexByte(decl, '('); open > 0 {
		name = strings.TrimSpace(decl[:open])
	}
	// Body ends at the first closing brace in column 0 after the offset.
	k := strings.Index(src[i:], "\n}")
	if k < 0 {
		return name, src[j:]
	}
	return name, src[j : i+k]
}

// moduleRoot walks up to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Skipf("cannot determine working directory: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skip("module root not found from this working directory")
	return ""
}

// PHASE 4 — the census must prove BOTH halves of the contract.
//
// Calling the authority is not enough. A handler that authorizes a path and
// then hands the STRING to os.OpenFile has re-resolved it, and the window the
// TOCTOU primitive exists to close is open again. Proven empirically: with
// RESOLVE_BENEATH removed, 35 of 800 opens escaped and read a file outside
// authorized storage while an ancestor was being swapped.
//
// So a function that authorizes AND performs a raw filesystem effect on a
// caller-influenced path fails here, by name.
var rawFilesystemEffect = regexp.MustCompile(
	`\b(os\.OpenFile|os\.Open|os\.Create|os\.Remove|os\.RemoveAll|os\.Rename|` +
		`os\.Mkdir|os\.MkdirAll|os\.ReadFile|os\.WriteFile)\(`)

var confinedPrimitive = regexp.MustCompile(
	`storageauth\.(OpenAuthorized|OpenAuthorizedStrict|OpenSystemStrict|ReadFileAuthorized|` +
		`StatAuthorized|RemoveAuthorized|RemoveSystemAuthorized|RemoveAllAuthorized|` +
		`RenameAuthorized|MkdirAllAuthorized|WalkRootAuthorized)\(`)

// rawEffectExemptions record functions that legitimately perform a raw effect
// beside an authorization, with the reason. Each is a reviewed decision.
var rawEffectExemptions = map[string]string{
	"PostFileUpload": "v1 upload writes into a temp directory it creates itself under the " +
		"authorized destination; migration to the confined primitive is tracked as F-01",
	"PostFileCompress": "archive output is written by the archiver library, which takes a path; " +
		"the destination is authorized and confined at creation",
	"PostCreateShareLink": "stats the share target only to record its size at creation; the " +
		"served path is re-authorized on every fetch",
	"GetFileSearch": "traversal root is authorized; migration to WalkRootAuthorized is tracked",
	"GetDownloadFile": "hands the path to echo's ctx.File, which owns the open; " +
		"tracked for migration to a confined reader",
}

func TestAuthorizedPathsReachTheConfinedPrimitive(t *testing.T) {
	root := moduleRoot(t)

	type row struct{ file, fn string }
	var offenders []row
	checked := 0

	for _, dir := range []string{"route", "service"} {
		_ = filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)

			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			src := string(data)

			for _, m := range regexp.MustCompile(`\nfunc [^\n]*\{`).FindAllStringIndex(src, -1) {
				end := strings.Index(src[m[1]:], "\n}")
				if end < 0 {
					continue
				}
				body := src[m[0] : m[1]+end]
				if !authorityCall.MatchString(body) {
					continue // not an authorized path at all
				}
				if !rawFilesystemEffect.MatchString(body) {
					continue // authorized and no raw effect: nothing to prove
				}
				checked++

				name := "?"
				if nm := regexp.MustCompile(`func (?:\([^)]*\)\s*)?(\w+)`).FindStringSubmatch(src[m[0]:m[1]]); nm != nil {
					name = nm[1]
				}
				if _, ok := rawEffectExemptions[name]; ok {
					continue
				}
				if confinedPrimitive.MatchString(body) {
					continue // reaches the primitive
				}
				offenders = append(offenders, row{rel, name})
			}
			return nil
		})
	}

	if len(offenders) > 0 {
		var lines []string
		for _, o := range offenders {
			lines = append(lines, o.file+"  func "+o.fn)
		}
		t.Fatalf("%d function(s) authorize a path and then perform a RAW filesystem effect on it:\n  %s\n\n"+
			"Authorizing and then handing the string to os.* re-resolves the path, which reopens the "+
			"TOCTOU window the confined primitive exists to close (measured: 35/800 escapes without "+
			"RESOLVE_BENEATH). Use storageauth.OpenAuthorizedStrict / RemoveAllAuthorized / "+
			"RenameAuthorized / MkdirAllAuthorized, or record the function in rawEffectExemptions "+
			"with its reason.",
			len(offenders), strings.Join(lines, "\n  "))
	}
	t.Logf("%d authorized function(s) with filesystem effects all reach the confined primitive "+
		"(%d exemption(s) on record)", checked, len(rawEffectExemptions))
}

// THE INERTNESS CENSUS.
//
// The storage-maintenance batch shipped complete, tested and mutation-verified
// — and inert. No engine was ever installed, so every mutating endpoint
// answered 503, RecoverInterruptedJobs never ran, and EnsureSanitizeSubsystem
// never ran. The API documented recovery guarantees that were, in that state,
// false.
//
// A subsystem that exists and is unreachable is this project's signature
// defect. It has now happened eight times, and it happened here inside the code
// built to destroy disks. So the wiring is asserted, not assumed.
//
// Mutation that must fail this test: delete the InstallStorageMaintenanceEngine
// call from main.go.
func TestStorageMaintenanceEngineIsInstalledAtStartup(t *testing.T) {
	root := moduleRoot(t)

	mainGo, err := os.ReadFile(filepath.Join(root, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(mainGo)

	if !strings.Contains(src, "InstallStorageMaintenanceEngine(") {
		t.Fatal("main.go never calls InstallStorageMaintenanceEngine; every destructive storage " +
			"endpoint would answer 503, interrupted formats would stay undiagnosed, and " +
			"sanitization artifacts left by a crash would never be cleaned up — while the API " +
			"documents the opposite")
	}

	// And the installer must actually do the three things the API promises,
	// rather than only registering an object.
	adapter, err := os.ReadFile(filepath.Join(root, "route/v1/maintenance_storage_engine.go"))
	if err != nil {
		t.Fatalf("read the adapter: %v", err)
	}
	a := string(adapter)
	for call, why := range map[string]string{
		"SetStorageMaintenanceEngine(": "the endpoints stay disabled",
		"RecoverInterruptedJobs(":      "an interrupted format is never diagnosed",
		"EnsureSanitizeSubsystem(":     "sanitize health reports unprovisioned forever and artifacts are never recovered",
		"DiscoverCapabilities(":        "a missing mkfs helper is discovered only after the user confirms a destructive operation",
	} {
		if !strings.Contains(a, call) {
			t.Fatalf("the installer never calls %s — %s", call, why)
		}
	}
}
