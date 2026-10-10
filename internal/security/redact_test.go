package security

import (
	"bytes"
	"strings"
	"testing"
)

// Realistic-shaped but fabricated credentials.
const (
	anthropicKey = "sk-ant-api03-Xk9fQ2mZ7vL0aB3cD4eF5gH6iJ7kL8mN9oP0qR1sT2uV3wX4yZ5aB6cD7eF8gH9iJ0kL1mN2oP3qR4sT5uV6wX7-AbCdEfGh"
	openaiLegacy = "sk-Zx8Vb2Nm4Kl6Jh8Gf0Ds2Aq4Wr6Et8Yu0Io2Pl4Kj6Hg8Fd"
	openaiProj   = "sk-proj-Abc123_DeF456-GhI789JkL012mNo345PqR678sTu901VwX234yZa567"
	googleKey    = "AIzaSyD-9tSrke72PouQMnMX-a7eZSW0jkFMBWY"
	githubPAT    = "ghp_1A2b3C4d5E6f7G8h9I0jK1lM2nO3pQ4rS5tU"
	githubFine   = "github_pat_11ABCDEFG0aBcDeFgHiJkL_mNoPqRsTuVwXyZ0123456789aBcDeFgHiJkLmNoPqRsTuVwXyZ01"
	bearerTok    = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	awsKey       = "AKIAIOSFODNN7EXAMPLE"
	pemKey       = "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7\nabcdefghijklmnopqrstuvwxyz0123456789\n-----END PRIVATE KEY-----"
	pemRSA       = "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA0Z3VS5JJcds3xfn\n-----END RSA PRIVATE KEY-----"
	pemOpenSSH   = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmU\n-----END OPENSSH PRIVATE KEY-----"
)

func TestRedactFormats(t *testing.T) {
	cases := map[string]string{
		"anthropic":   anthropicKey,
		"openai":      openaiLegacy,
		"openai-proj": openaiProj,
		"google":      googleKey,
		"github-pat":  githubPAT,
		"github-fine": githubFine,
		"aws":         awsKey,
		"pem":         pemKey,
		"pem-rsa":     pemRSA,
		"pem-openssh": pemOpenSSH,
	}
	for name, secret := range cases {
		t.Run(name, func(t *testing.T) {
			in := "before " + secret + " after"
			out := Redact(in)
			if strings.Contains(out, secret) || strings.Contains(out, secret[:min(len(secret), 24)]) {
				t.Fatalf("secret survived: %q", out)
			}
			if !strings.HasPrefix(out, "before ") || !strings.HasSuffix(out, " after") || !strings.Contains(out, "[REDACTED:") {
				t.Fatalf("unexpected output %q", out)
			}
		})
	}
}

func TestRedactBearer(t *testing.T) {
	out := Redact("Authorization: Bearer " + bearerTok)
	if strings.Contains(out, bearerTok[:20]) {
		t.Fatalf("bearer survived: %q", out)
	}
	if !strings.Contains(out, "Bearer [REDACTED:bearer]") {
		t.Fatalf("want bearer marker, got %q", out)
	}
	if got := Redact("bearer  abcdefgh12345"); strings.Contains(got, "abcdefgh") {
		t.Fatalf("case-insensitive bearer survived: %q", got)
	}
}

func TestRedactAnthropicNotMislabelled(t *testing.T) {
	if out := Redact(anthropicKey); out != "[REDACTED:anthropic-key]" {
		t.Fatalf("got %q", out)
	}
}

func TestRedactLeavesBenignText(t *testing.T) {
	for _, s := range []string{
		"risk-assessment-of-something-quite-long-indeed",
		"task-list sk-short",
		"Bearer",
		"the quick brown fox",
		"ghp_tooshort",
	} {
		if out := Redact(s); out != s {
			t.Errorf("over-redacted %q -> %q", s, out)
		}
	}
}

func TestRedactJSONEscapedPEM(t *testing.T) {
	esc := strings.ReplaceAll(pemKey, "\n", `\n`)
	if out := Redact(`{"msg":"` + esc + `"}`); strings.Contains(out, "MIIEvQ") {
		t.Fatalf("escaped PEM survived: %q", out)
	}
}

func TestWriterSplitAcrossWrites(t *testing.T) {
	var sink bytes.Buffer
	w := NewWriter(&sink)
	in := "key=" + anthropicKey + "\nnext line " + googleKey + "\n" + pemKey + "\ntail " + githubPAT
	for i := 0; i < len(in); i += 7 {
		end := min(i+7, len(in))
		if _, err := w.Write([]byte(in[i:end])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out := sink.String()
	for _, s := range []string{anthropicKey[:30], googleKey[:20], "MIIEvQ", githubPAT[:20]} {
		if strings.Contains(out, s) {
			t.Fatalf("secret fragment %q survived:\n%s", s, out)
		}
	}
	if !strings.Contains(out, "key=[REDACTED:anthropic-key]\n") || !strings.Contains(out, "tail [REDACTED:github-token]") {
		t.Fatalf("unexpected stream:\n%s", out)
	}
}

func TestWriterUnterminatedPEMRedactedOnClose(t *testing.T) {
	var sink bytes.Buffer
	w := NewWriter(&sink)
	_, _ = w.Write([]byte("pre\n-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBg"))
	if strings.Contains(sink.String(), "MIIEvQ") {
		t.Fatal("leaked before close")
	}
	_ = w.Close()
	if strings.Contains(sink.String(), "MIIEvQ") || !strings.Contains(sink.String(), "[REDACTED:pem-private-key]") {
		t.Fatalf("got %q", sink.String())
	}
}

func TestWriterLongLineWithoutNewline(t *testing.T) {
	var sink bytes.Buffer
	w := NewWriter(&sink)
	blob := strings.Repeat("a ", maxLine/2) + anthropicKey + strings.Repeat(" b", 100)
	_, _ = w.Write([]byte(blob))
	_ = w.Close()
	if strings.Contains(sink.String(), anthropicKey[:30]) {
		t.Fatal("secret survived long line")
	}
}

// Board review #5 M2: the ops run ticket as env/printenv/export -p print it.
func TestRedactRunTicket(t *testing.T) {
	tok := strings.Repeat("0123456789abcdef", 4)
	for _, in := range []string{
		"STAYPOINT_RUN_TICKET=" + tok,
		"PATH=/bin\nSTAYPOINT_RUN_TICKET=" + tok + "\nHOME=/x",
		`declare -x STAYPOINT_RUN_TICKET="` + tok + `"`,
		"STAYPOINT_RUN_TICKET=" + strings.ToUpper(tok),
	} {
		if out := Redact(in); strings.Contains(strings.ToLower(out), tok[:16]) || !strings.Contains(out, "[REDACTED:run-ticket]") {
			t.Errorf("ticket survived: %q", out)
		}
	}
	if out := Redact("STAYPOINT_TASK_ID=task-7d279c9d"); out != "STAYPOINT_TASK_ID=task-7d279c9d" {
		t.Errorf("over-redacted task id: %q", out)
	}
}
