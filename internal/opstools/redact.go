package opstools

import (
	"regexp"

	"github.com/VinnyVanGogh/staypoint/internal/security"
)

var (
	// secretAssignRe masks the value of a secret-looking assignment, as an
	// env file, a settings dump or a log line prints it: SECRET_KEY=...,
	// export DB_PASSWORD="...", "api_key": "...".
	secretAssignRe = regexp.MustCompile(`(?i)(\b[A-Za-z0-9_.-]*(?:secret|passw(?:or)?d|passwd|token|api_?key|private_?key|_key|credential|dsn|auth)[A-Za-z0-9_.-]*["']?\s*[=:]\s*)("[^"\n]*"|'[^'\n]*'|[^\s,;}]+)`)
	// urlCredRe masks the password in scheme://user:password@host.
	urlCredRe = regexp.MustCompile(`(://[^/\s:@]+:)[^@\s/]+@`)
)

// Redact masks secrets in tool output: everything security.Redact knows,
// plus secret-named assignments and passwords inside URLs.
func Redact(s string) string {
	s = security.Redact(s)
	s = urlCredRe.ReplaceAllString(s, "${1}[REDACTED:url-password]@")
	s = secretAssignRe.ReplaceAllString(s, "${1}[REDACTED:secret]")
	return s
}

// maxOutput caps what a tool returns to the agent.
const maxOutput = 64 << 10

// capOutput keeps the first maxOutput bytes and says so when it cut.
func capOutput(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	return s[:maxOutput] + "\n[output truncated]"
}
