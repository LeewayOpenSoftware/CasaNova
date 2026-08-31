package v2

// Three mutating endpoints were added for the ISO and disk sidebar: delete an
// ISO, create a disk, delete a disk. Each destroys or allocates real storage,
// so each has to refuse a caller without configure rights.
//
// The invariant test next door proves the GATE aborts a handler; it says
// nothing about whether a given handler has one. That is the gap this closes,
// and it is the gap that has bitten this codebase before — handlers shipped
// with no gate at all, which no amount of testing the gate would have caught.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestNewStorageHandlers_RefuseAGuest(t *testing.T) {
	// A handler that reached its service would panic on the nil dependency,
	// so "no panic and a refusal" is the assertion: the gate returned first.
	h := &VirtHandler{}

	cases := []struct {
		name    string
		method  string
		params  map[string]string
		body    string
		handler func(echo.Context) error
	}{
		{"DeleteISO", http.MethodDelete, map[string]string{"name": "x.iso"}, "", h.DeleteISO},
		{"CreateVolume", http.MethodPost, map[string]string{"name": "default"}, `{"name":"d","capacity":1}`, h.CreateVolume},
		{"DeleteVolume", http.MethodDelete, map[string]string{"name": "default", "volume": "d"}, "", h.DeleteVolume},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := echo.New()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(c.method, "/", strings.NewReader(c.body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			// No role on the context resolves to guest.
			ctx := e.NewContext(req, rec)
			for k, v := range c.params {
				ctx.SetParamNames(append(ctx.ParamNames(), k)...)
				ctx.SetParamValues(append(ctx.ParamValues(), v)...)
			}

			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s reached its service before refusing: %v", c.name, r)
				}
			}()

			err := c.handler(ctx)
			if err == nil && rec.Code < 400 {
				t.Fatalf("%s allowed a guest: code=%d body=%s", c.name, rec.Code, rec.Body.String())
			}
		})
	}
}
