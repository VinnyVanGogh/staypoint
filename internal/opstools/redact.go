package opstools

import (
	"regexp"

	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// secretName matches a secret-looking variable or key name. A bare "pass"
// is left out on purpose: verify output prints "PASS: ..." lines.
const secretName = `(?:[A-Za-z0-9_.-]*(?:secret|passw(?:or)?d|passwd|pwd|token|credential|dsn|auth|api_?key|private_?key|access_?key)[A-Za-z0-9_.-]*` +
	`|[A-Za-z0-9_.-]*[_.-](?:key|pass)|key)`

// secretValue is the value after the separator, first match wins: a Python
// triple-quoted string (to its end, or the end of the output), a quoted
// string with backslash escapes, else everything to the end of the line
// (which also covers an unterminated quote).
const secretValue = `"""[\s\S]*?(?:"""|\z)|'''[\s\S]*?(?:'''|\z)|"(?:[^"\\\r\n]|\\.)*"|'(?:[^'\\\r\n]|\\.)*'|[^\r\n]+`

var (
	// secretAssignRe masks the value of a secret-looking assignment, as an
	// env file, a settings dump or a log line prints it: SECRET_KEY=...,
	// export DB_PASSWORD="...", "api_key": "...", key = 'a b'.
	secretAssignRe = regexp.MustCompile(`(?i)(\b` + secretName + `["']?[ \t]*[=:][ \t]*)(` + secretValue + `)`)
	// urlCredRe masks the password in scheme://user:password@host, with an
	// empty user (redis://:pw@host) and up to the last @ before the path
	// (user:p@ss@host).
	urlCredRe = regexp.MustCompile(`(://[^/\s:@]*:)[^\s/?#]*@`)
	// pemKeyRe masks a private key block, to the end of the output when the
	// END line is missing (a capped cat_file can cut it off).
	pemKeyRe = regexp.MustCompile(`-----BEGIN[A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(?:-----END[A-Z0-9 ]*PRIVATE KEY-----|\z)`)
)

// Redact masks secrets in tool output: everything security.Redact knows,
// plus private keys, secret-named assignments and passwords inside URLs.
func Redact(s string) string {
	s = pemKeyRe.ReplaceAllString(s, "[REDACTED:private-key]")
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
