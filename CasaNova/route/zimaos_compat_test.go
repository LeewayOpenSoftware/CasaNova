package route

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// The client gates on these fields; dropping one silently breaks connect.
func TestZimaDeviceInfo_HasGateFields(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/v2/zimaos/device/info", nil), rec)
	if err := zimaDeviceInfo(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// The response is FLAT by wire contract — the Zima Android client does
	// Gson.fromJson(body, DeviceInfo.class) on the top level; a {"data":{...}}
	// envelope nulls every field and the client shows "cannot connect".
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"initialized", "os_version", "min_client_version", "device_name", "arch"} {
		if _, ok := body[k]; !ok {
			t.Errorf("device/info missing gate field %q", k)
		}
	}
	if body["initialized"] != true {
		t.Error("initialized must be true or clients treat the box as un-setup")
	}
}
