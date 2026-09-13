package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/service"
	"github.com/glebarez/sqlite"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// auditAdminCtx builds the request an administrator's browser makes when it
// opens the audit page, already past the admin gate.
func auditAdminCtx(t *testing.T) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/sys/audit?limit=500", nil)
	rec := httptest.NewRecorder()
	ctx := e.NewContext(req, rec)
	commonmw.SetRoleInContext(ctx, string(rbac.RoleAdmin), 1)
	return ctx, rec
}

// auditDataOf runs the handler and returns the decoded `data` object.
func auditDataOf(t *testing.T) map[string]interface{} {
	t.Helper()
	ctx, rec := auditAdminCtx(t)
	if err := GetAuditEvents(ctx); err != nil {
		t.Fatalf("handler returned an error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 so the audit page still renders, got %d: %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("response is not the {success,data} envelope: %v (%s)", err, rec.Body.String())
	}
	return envelope.Data
}

// initAuditStore points the process audit logger at a throwaway database, so
// the reader is live and the table is genuinely empty.
func initAuditStore(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	service.InitAuditAndNotifications(db)
}

// "NOTHING HAPPENED" AND "NOTHING WAS EVER RECORDED" MUST NOT LOOK THE SAME.
//
// The read side of the audit log is wired (main.go calls
// InitAuditAndNotifications), so GET /v1/sys/audit answers 200. The write side
// is not: CasaNova-UserService's SetAuditLoggerDB has no caller, and this
// process has no audit writers at all. The table is therefore empty by
// construction, and the endpoint reported that as an ordinary empty result.
//
// An administrator opening the audit page then reads "no security events" off a
// console that has never recorded one. That is a false all-clear on a security
// surface, which is worse than an error: an error gets investigated.
//
// The endpoint must keep answering 200 -- the only first-party consumer,
// AuditSettings.vue, calls safeCall() with no fallback and dereferences
// result.data.events without checking result.error, so a 503 would throw a
// TypeError and take the page down. The truth has to travel IN the payload.
func TestAnUnrecordedAuditLogIsNotReportedAsAnEmptyOne(t *testing.T) {
	initAuditStore(t)

	data := auditDataOf(t)

	recording, present := data["recording"]
	if !present {
		t.Fatal("the response does not say whether anything is being recorded, so an empty " +
			"list is indistinguishable from a log that never received an event -- which is " +
			"exactly the false all-clear this endpoint was giving")
	}
	if recording != false {
		t.Fatalf("nothing records into this process's audit store, but the endpoint claims it "+
			"does: recording=%v", recording)
	}
	if _, ok := data["recording_detail"]; !ok {
		t.Fatal("recording is false but nothing explains why, so the operator is told the log " +
			"is off without being told what to do about it")
	}
	if data["count"] != float64(0) {
		t.Fatalf("precondition: the store should be empty, got count=%v", data["count"])
	}
}

// The other half, and the one that proves the first is not just a constant:
// once recording IS wired, an empty log must read as an ordinary empty log.
// Without this, "always say it is off" would pass the test above.
func TestAWiredAuditLogWithNoEventsReadsAsAnOrdinaryEmptyLog(t *testing.T) {
	initAuditStore(t)
	service.MarkAuditRecordingWired()

	data := auditDataOf(t)

	if data["recording"] != true {
		t.Fatalf("recording is wired but the endpoint still reports it off: recording=%v",
			data["recording"])
	}
	if _, ok := data["recording_detail"]; ok {
		t.Fatal("a healthy audit log should not carry a fault explanation")
	}
	if data["count"] != float64(0) {
		t.Fatalf("want an empty-but-healthy log, got count=%v", data["count"])
	}
}
