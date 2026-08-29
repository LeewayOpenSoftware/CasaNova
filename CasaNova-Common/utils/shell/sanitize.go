// Package shell provides safe helpers for constructing shell command arguments.
//
// CasaNova has historically invoked shell scripts by building commands from
// user-supplied strings — a classic injection vector. This package centralizes
// the sanitization rules so there's one place to audit and harden.
package shell

// SanitizeArg strips every character except a conservative safe set:
// alphanumeric, dash, underscore, dot, and forward slash. Anything else
// (spaces, quotes, backticks, semicolons, ampersands, pipes, etc.) is dropped.
//
// This is deliberately strict. It accepts valid device paths (/dev/sda1),
// network interface names (eth0, wlan0), hostnames (my-server.local), and
// filesystem paths, but rejects anything that could chain commands or break
// out of the intended argument.
//
// Callers that need to preserve other characters (spaces in filenames, etc.)
// should use exec.Command with separate arguments instead of shell strings.
func SanitizeArg(s string) string {
	result := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '/' {
			result = append(result, c)
		}
	}
	return string(result)
}
