package gates

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/VinnyVanGogh/staypoint/internal/security"
)

func TestDescribeRequestBoundsLargeScripts(t *testing.T) {
	big := "#!/bin/bash\n# HEAD-MARK\n" + strings.Repeat("echo \"line of a long script\" | grep -v x\n", 1250) + "# TAIL-MARK\n"
	if len(big) < 50_000 {
		t.Fatalf("fixture is %d bytes, want >= 50 KB", len(big))
	}
	gr := &security.GateRequest{Cmdline: strings.Repeat("a", 9000) + " bash big.sh", Reasons: []string{"runs a script"}, Repo: "/r", CWD: "/r"}
	scripts := []security.ScriptRef{
		{Path: "/r/big.sh", SHA256: strings.Repeat("ab", 32), Content: big},
		{Path: "/r/also.sh", SHA256: strings.Repeat("cd", 32), Content: big},
	}
	d := DescribeRequest(gr, scripts)
	if len(d) > DescribeLimit {
		t.Fatalf("description is %d bytes, want <= %d", len(d), DescribeLimit)
	}
	for _, want := range []string{"Script /r/big.sh (sha256 abababababab)", "Script /r/also.sh (sha256 cdcdcdcdcdcd)", "HEAD-MARK", "TAIL-MARK", "bash big.sh", "…(truncated)…"} {
		if !strings.Contains(d, want) {
			t.Errorf("description lacks %q", want)
		}
	}
	if !utf8.ValidString(d) {
		t.Error("description is not valid UTF-8")
	}
}

func TestDescribeRequestManyScriptsStayBounded(t *testing.T) {
	var scripts []security.ScriptRef
	for i := 0; i < 200; i++ {
		scripts = append(scripts, security.ScriptRef{Path: "/r/" + strings.Repeat("p", 60) + ".sh", SHA256: "ff", Content: strings.Repeat("ü", 3000)})
	}
	d := DescribeRequest(&security.GateRequest{Cmdline: "bash x.sh"}, scripts)
	if len(d) > DescribeLimit || !utf8.ValidString(d) {
		t.Fatalf("len %d valid %v", len(d), utf8.ValidString(d))
	}
}

func TestDescribeRequestHidesBinaries(t *testing.T) {
	macho := "\xcf\xfa\xed\xfe\x0c\x00\x00\x01__TEXT" + strings.Repeat("\x00\x01", 2000)
	elf := "\x7fELF\x02\x01\x01\x00" + strings.Repeat("\x00", 100)
	scripts := []security.ScriptRef{
		{Path: "/opt/homebrew/bin/pg_ctl", SHA256: strings.Repeat("12", 32), Content: macho},
		{Path: "/r/tool", SHA256: strings.Repeat("34", 32), Content: elf},
	}
	d := DescribeRequest(&security.GateRequest{Cmdline: "/opt/homebrew/bin/pg_ctl status"}, scripts)
	for _, want := range []string{
		"Script /opt/homebrew/bin/pg_ctl (sha256 121212121212): binary, not shown",
		"Script /r/tool (sha256 343434343434): binary, not shown",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("description lacks %q:\n%s", want, d)
		}
	}
	if strings.Contains(d, "__TEXT") || strings.Contains(d, "ELF") || strings.IndexByte(d, 0) >= 0 {
		t.Errorf("binary content leaked:\n%q", d)
	}
}

// Bytes bash would still run must not hide a script from the advisor.
func TestDescribeRequestShowsScriptsWithStrayBytes(t *testing.T) {
	scripts := []security.ScriptRef{
		{Path: "/r/latin1.sh", SHA256: "aa", Content: "echo caf\xe9\nrm -rf ~/EVIL1\n"},
		{Path: "/r/nul.sh", SHA256: "bb", Content: "echo ok\nx=\x00\nrm -rf ~/EVIL2\n"},
		{Path: "/r/she.sh", SHA256: "cc", Content: "#!/bin/sh\x00\nrm -rf ~/EVIL3\n"},
		{Path: "/r/json.sh", SHA256: "dd", Content: "echo \uFFFD\nrm -rf ~/EVIL4\n"},
	}
	d := DescribeRequest(&security.GateRequest{Cmdline: "bash x \xff\x00"}, scripts)
	for _, want := range []string{"EVIL1", "EVIL2", "EVIL3", "EVIL4"} {
		if !strings.Contains(d, want) {
			t.Errorf("description hides %s:\n%s", want, d)
		}
	}
	if strings.Contains(d, "binary") || !utf8.ValidString(d) || strings.IndexByte(d, 0) >= 0 {
		t.Errorf("want valid, NUL-free text with no binary label:\n%q", d)
	}
}

func TestLooksBinary(t *testing.T) {
	for _, s := range []string{"#!/bin/sh\necho héllo ✓\n", "#!\x00", "echo\n\x00", strings.Repeat("a", 80) + "\x00", "a\xffb"} {
		if looksBinary(s) {
			t.Errorf("looksBinary(%q) = true, want false", s)
		}
	}
	for _, s := range []string{"a\x00b", "\x7fELF\x02\x01\x01\x00", "MZ\x90\x00"} {
		if !looksBinary(s) {
			t.Errorf("looksBinary(%q) = false, want true", s)
		}
	}
}

func TestHeadTail(t *testing.T) {
	if got := headTail("short", 10); got != "short" {
		t.Errorf("short: %q", got)
	}
	s := strings.Repeat("✓", 100)
	for _, n := range []int{-5, 0, 3, 17, 18, 40, 299} {
		got := headTail(s, n)
		if len(got) > max(n, 0) || !utf8.ValidString(got) {
			t.Errorf("headTail(n=%d) = %q (%d bytes)", n, got, len(got))
		}
	}
}
