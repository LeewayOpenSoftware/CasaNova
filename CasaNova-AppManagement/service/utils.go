package service

import (
	"regexp"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils"
	"gopkg.in/yaml.v3"
)

var nonAlphaNumeric = regexp.MustCompile(`[^a-z0-9]+`)

func Standardize(text string) string {
	if text == "" {
		return "unknown"
	}

	result := strings.ToLower(text)

	// Replace any non-alphanumeric characters with a single hyphen
	result = nonAlphaNumeric.ReplaceAllString(result, "-")

	for strings.Contains(result, "--") {
		result = strings.Replace(result, "--", "-", -1)
	}

	// Remove any leading or trailing hyphens
	result = strings.Trim(result, "-")

	return result
}

// escapeDollarSigns escapes unescaped $ characters in a string for Docker Compose YAML.
// Single $ becomes $$ (escaped for compose interpolation).
// Already-escaped $$ is left untouched to prevent the doubling-on-every-save bug.
func escapeDollarSigns(s string) string {
	var result strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '$' {
			if i+1 < len(s) && s[i+1] == '$' {
				// Already escaped ($$), write both and skip next
				result.WriteString("$$")
				i++
			} else {
				// Single $, escape it
				result.WriteString("$$")
			}
		} else {
			result.WriteByte(s[i])
		}
	}
	return result.String()
}

func GenerateYAMLFromComposeApp(compose ComposeApp) ([]byte, error) {
	// Escape single $ in env values for Docker Compose YAML interpolation.
	// Previously this used strings.Replace($, $$) which doubled ALL dollar signs
	// on every save, causing progressive corruption ($ → $$ → $$$$ → ...).
	for _, service := range compose.Services {
		for key, value := range service.Environment {
			if value != nil && strings.Contains(*value, "$") {
				service.Environment[key] = utils.Ptr(escapeDollarSigns(*value))
			}
		}
	}
	return yaml.Marshal(compose)
}
