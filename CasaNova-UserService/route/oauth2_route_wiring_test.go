package route

import (
	"os"
	"regexp"
	"testing"
)

// The two OAuth endpoints deliberately use different issuer gates. A
// first-party CasaNova access token may authorize consent, while an OAuth
// delegated token is confined to userinfo. Validator unit tests alone do not
// prove that the router keeps each validator attached to the right endpoint.
func TestOAuth2EndpointsUseDistinctJWTValidators(t *testing.T) {
	source, err := os.ReadFile("v1.go")
	if err != nil {
		t.Fatalf("read router source: %v", err)
	}

	checks := map[string]string{
		"delegated config validates only delegated tokens": `delegatedJWTConfig\.ParseTokenFunc\s*=\s*func[\s\S]*?jwt\.ValidateDelegated\(`,
		"authorize group uses the general validator":       `v1OAuth2Authorize\.Use\(echo_middleware\.JWTWithConfig\(jwtConfig\)\)[\s\S]*?v1OAuth2Authorize\.GET\("/authorize"`,
		"userinfo group uses the delegated validator":      `v1OAuth2UserInfo\.Use\(echo_middleware\.JWTWithConfig\(delegatedJWTConfig\)\)[\s\S]*?v1OAuth2UserInfo\.GET\("/userinfo"`,
	}
	for property, pattern := range checks {
		if !regexp.MustCompile(pattern).Match(source) {
			t.Errorf("OAuth2 route wiring lost property: %s", property)
		}
	}
}
