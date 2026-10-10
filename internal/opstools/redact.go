package opstools

import (
	"regexp"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// secretName matches a secret-looking variable or key name. A bare "pass"
// is left out on purpose: verify output prints "PASS: ..." lines.
const secretName = `(?:[A-Za-z0-9_.-]*(?:secret|passw(?:or)?d|passwd|pwd|token|credential|dsn|auth|api_?key|private_?key|access_?key|salt|session_?id|cookie)[A-Za-z0-9_.-]*` +
	`|[A-Za-z0-9_.-]*[_.-](?:key|pass|pw|sk)|key|pw)`

// secretValue is the value after the separator, first match wins: a Python
// triple-quoted string (to its end, or the end of the output), a quoted
// string with backslash escapes, else everything to the end of the line,
// following shell line continuations (PASSWORD=x \ <newline> more), which
// also covers an unterminated quote.
const secretValue = `"""[\s\S]*?(?:"""|\z)|'''[\s\S]*?(?:'''|\z)|"(?:[^"\\\r\n]|\\.)*"|'(?:[^'\\\r\n]|\\.)*'|(?:[^\r\n]*\\\r?\n)*[^\r\n]+`

var (
	// secretAssignRe masks the value of a secret-looking assignment, as an
	// env file, a settings dump or a log line prints it: SECRET_KEY=...,
	// export DB_PASSWORD="...", "api_key": "...", key = 'a b'.
	secretAssignRe = regexp.MustCompile(`(?i)(\b` + secretName + `["']?[ \t]*[=:][ \t]*)(` + secretValue + `)`)
	// urlCredRe finds the password in scheme://user:password@host, with an
	// empty user (redis://:pw@host), up to the last @ in the URL
	// (user:p@ss@host), and with / or ? in it (user:pa/ss@host).
	urlCredRe = regexp.MustCompile(`(://[^/\s:@'"<>]*:)([^\s'"<>]*)@`)
	// urlPortRe is what urlCredRe found when the "password" is really a
	// port and path (http://host:8000/next?u=a@b): that is left alone.
	urlPortRe = regexp.MustCompile(`^[0-9]{1,5}(?:[/?#]|$)`)
	// mysqlPassRe masks mysql -pPASSWORD (no space: -p alone prompts, and
	// -P is the port).
	mysqlPassRe = regexp.MustCompile(`(\b(?i:mysql(?:dump|admin|import|show|check)?)\b[^\r\n]*?\s-p)('[^'\r\n]*'?|"[^"\r\n]*"?|[^\s'"]+)`)
	// pemKeyRe masks a private key block, to the end of the output when the
	// END line is missing (a capped cat_file can cut it off).
	pemKeyRe = regexp.MustCompile(`-----BEGIN[A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(?:-----END[A-Z0-9 ]*PRIVATE KEY-----|\z)`)
)

// Redact masks secrets in tool output: everything security.Redact knows,
// plus private keys, secret-named assignments and passwords inside URLs.
func Redact(s string) string {
	s = pemKeyRe.ReplaceAllString(s, "[REDACTED:private-key]")
	s = security.Redact(s)
	s = urlCredRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := urlCredRe.FindStringSubmatch(m)
		// A host (dotted, or localhost) then digits then a path is a
		// port; a plain user name before a digit password is not.
		host := strings.TrimSuffix(strings.TrimPrefix(sub[1], "://"), ":")
		if (strings.Contains(host, ".") || host == "localhost") && urlPortRe.MatchString(sub[2]) {
			return m
		}
		return sub[1] + "[REDACTED:url-password]@"
	})
	s = mysqlPassRe.ReplaceAllString(s, "${1}[REDACTED:secret]")
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
