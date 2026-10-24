package downloadHelper

import (
	"net/http"
	"testing"
)

// CheckRedirect must refuse an https -> http downgrade while still allowing
// same-scheme and upgrading redirects. Public IP literals keep the SSRF
// guard from deciding the outcome and need no DNS or network.
func TestSafeClientRefusesHTTPSToHTTPRedirect(t *testing.T) {
	c := newSafeHTTPClient()
	mk := func(u string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	cases := []struct {
		from, to string
		wantErr  bool
	}{
		{"https://8.8.8.8/main.zip", "http://8.8.4.4/main.zip", true},
		{"https://8.8.8.8/main.zip", "HTTP://8.8.4.4/main.zip", true},
		{"https://8.8.8.8/main.zip", "https://8.8.4.4/main.zip", false},
		{"http://8.8.8.8/main.zip", "https://8.8.4.4/main.zip", false},
		{"http://8.8.8.8/main.zip", "http://8.8.4.4/main.zip", false},
	}
	for _, tc := range cases {
		err := c.CheckRedirect(mk(tc.to), []*http.Request{mk(tc.from)})
		if (err != nil) != tc.wantErr {
			t.Errorf("redirect %s -> %s: err = %v, wantErr %v", tc.from, tc.to, err, tc.wantErr)
		}
	}
}
