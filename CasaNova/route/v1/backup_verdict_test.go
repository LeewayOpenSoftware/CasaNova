package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/labstack/echo/v4"
)

// withUnreadableSource points the backup at a source directory that cannot be
// read, entirely inside the test's own temporary directory.
//
// Nothing here touches the real /var/lib/casaos: backupDir is redirected too,
// so a handler that decides to write a tarball writes it under t.TempDir().
//
// The walk is substituted for the same reason the existing skip-reporting test
// substitutes it — a suite running as root can read a 0o000 directory, and the
// verdict under test must not depend on the uid of whoever runs `go test`. The
// substitute still refuses exactly what the kernel would refuse.
func withUnreadableSource(t *testing.T) (dir string, blocked string) {
	t.Helper()

	root := t.TempDir()
	blocked = filepath.Join(root, "conf")
	if err := os.Mkdir(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })

	oldSources, oldWalk, oldDir := backupSources, backupWalk, backupDir
	backupSources = []string{blocked}
	backupDir = filepath.Join(root, "backups")
	backupWalk = func(path string, walkFn filepath.WalkFunc) error {
		info, err := os.Stat(path)
		if err != nil {
			return walkFn(path, nil, err)
		}
		if info.IsDir() && info.Mode().Perm()&0o400 == 0 {
			return walkFn(path, nil, &os.PathError{Op: "open", Path: path, Err: syscall.EACCES})
		}
		return filepath.Walk(path, walkFn)
	}
	t.Cleanup(func() {
		backupSources, backupWalk, backupDir = oldSources, oldWalk, oldDir
	})

	return root, blocked
}

// createBackup drives the real handler as an administrator and hands back the
// decoded envelope.
func createBackup(t *testing.T) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	e := echo.New()
	rec := httptest.NewRecorder()
	ctx := e.NewContext(httptest.NewRequest(http.MethodPost, "/v1/sys/backup/create", nil), rec)
	commonmw.SetRoleInContext(ctx, "admin", 1)

	if err := PostBackupCreate(ctx); err != nil {
		t.Fatalf("handler returned an error instead of a response: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (%q)", err, rec.Body.String())
	}
	return rec, body
}

// THE DECISION UNDER GUARD: a backup that could not read everything is a
// failure. Not a success with warnings, not a success with a populated
// "skipped" array — a failure.
//
// This test is deliberately written as the tripwire for that decision rather
// than as a test of the mechanism. If someone later restores the
// success-with-warnings behaviour, whether by reverting the verdict or by
// adding a threshold or an exemption, this goes red and names the decision it
// is protecting. The skip-reporting half is already covered by
// TestCreateBackupTarballReportsUnreadableDirectory and is not retested here.
func TestPartialBackupIsNeverReportedAsSuccess(t *testing.T) {
	withUnreadableSource(t)

	rec, body := createBackup(t)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d: a backup that skipped a path answered success",
			rec.Code, http.StatusInternalServerError)
	}
	success, _ := body["success"].(float64)
	if int(success) != common_err.SERVICE_ERROR {
		t.Fatalf("success = %v, want %d (SERVICE_ERROR): the partial backup was reported as a completed job",
			body["success"], common_err.SERVICE_ERROR)
	}
}

// A failed job must not leave an artefact that the rest of the system treats
// as a real backup: GetBackupList globs the directory, and restoring the
// leftover would put an incomplete system back silently.
func TestFailedBackupLeavesNoTarballBehind(t *testing.T) {
	withUnreadableSource(t)

	if _, _ = createBackup(t); true {
		matches, err := filepath.Glob(filepath.Join(backupDir, "backup-*.tar.gz"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 0 {
			t.Fatalf("failed backup left %d tarball(s) behind: %v", len(matches), matches)
		}
	}
}

// The strict verdict is only survivable if the failure says what caused it.
// One unreadable scratch file on a live box can fail every nightly run, and
// whoever looks at that failure has to be able to tell junk from a real loss
// without opening a log or unpacking the archive to read its manifest.
func TestBackupFailureNamesThePathAndTheReason(t *testing.T) {
	_, blocked := withUnreadableSource(t)

	_, body := createBackup(t)

	message, _ := body["message"].(string)
	if !strings.Contains(message, blocked) {
		t.Fatalf("failure message does not name the offending path %q: %q", blocked, message)
	}
	if !strings.Contains(message, "permission denied") {
		t.Fatalf("failure message does not give the reason: %q", message)
	}
}

// The count and the summary have to hold for more than one casualty too: the
// message names the first and says how many others there were, so a single
// piece of junk is distinguishable at a glance from a whole subtree going
// missing.
func TestBackupIncompleteReasonSummarisesTheRest(t *testing.T) {
	if got := backupIncompleteReason(nil); got != "" {
		t.Fatalf("a backup that skipped nothing reported %q, want no failure", got)
	}

	one := backupIncompleteReason([]backupSkipped{{Path: "/etc/casaos", Reason: "permission denied"}})
	if !strings.Contains(one, "1 path could not be read") || !strings.Contains(one, "/etc/casaos") {
		t.Fatalf("single-casualty message = %q", one)
	}
	if strings.Contains(one, "and 0 more") {
		t.Fatalf("single-casualty message invented a tail: %q", one)
	}

	many := backupIncompleteReason([]backupSkipped{
		{Path: "/etc/casaos", Reason: "permission denied"},
		{Path: "/var/lib/casaos/db", Reason: "permission denied"},
		{Path: "/var/lib/casaos/conf", Reason: "permission denied"},
	})
	if !strings.Contains(many, "3 paths could not be read") {
		t.Fatalf("multi-casualty message lost the count: %q", many)
	}
	if !strings.Contains(many, "/etc/casaos") || !strings.Contains(many, "and 2 more") {
		t.Fatalf("multi-casualty message = %q", many)
	}
}
