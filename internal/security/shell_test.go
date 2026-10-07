package security

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseShellBasic(t *testing.T) {
	line := "echo hello   world  'extra arg'"
	segs, subs, err := parseShell(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(subs) != 0 {
		t.Errorf("expected 0 subs, got %v", subs)
	}
	if len(segs) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segs))
	}
	expectedArgv := []string{"echo", "hello", "world", "extra arg"}
	if !reflect.DeepEqual(segs[0].argv, expectedArgv) {
		t.Errorf("expected argv %v, got %v", expectedArgv, segs[0].argv)
	}
	if segs[0].piped {
		t.Errorf("expected piped to be false")
	}
}

func TestParseShellQuoting(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantArgv  []string
		wantError string
	}{
		{
			name:     "single quote literal",
			line:     `echo 'hello "world" $var'`,
			wantArgv: []string{"echo", `hello "world" $var`},
		},
		{
			name:      "unterminated single quote",
			line:      `echo 'unterminated`,
			wantError: "unterminated single quote",
		},
		{
			name:     "double quote with escaped quotes and backslashes",
			line:     `echo "hello \"world\" \\ test"`,
			wantArgv: []string{"echo", `hello "world" \ test`},
		},
		{
			name:      "unterminated double quote",
			line:      `echo "unterminated`,
			wantError: "unterminated double quote",
		},
		{
			name:     "unquoted escaped characters",
			line:     `echo hello\ world escaped\\path`,
			wantArgv: []string{"echo", "hello world", `escaped\path`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			segs, _, err := parseShell(tt.line)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("expected error containing %q, got %v", tt.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(segs) != 1 {
				t.Fatalf("expected 1 segment, got %d", len(segs))
			}
			if !reflect.DeepEqual(segs[0].argv, tt.wantArgv) {
				t.Errorf("argv mismatch: got %v, want %v", segs[0].argv, tt.wantArgv)
			}
		})
	}
}

func TestParseShellCommandSubstitution(t *testing.T) {
	line := `echo $(whoami) "current: $(date -u)"`
	segs, subs, err := parseShell(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedSubs := []string{"whoami", "date -u"}
	if !reflect.DeepEqual(subs, expectedSubs) {
		t.Errorf("subs mismatch: got %v, want %v", subs, expectedSubs)
	}

	if len(segs) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segs))
	}
	expectedArgv := []string{"echo", "$SUBST", "current: $SUBST"}
	if !reflect.DeepEqual(segs[0].argv, expectedArgv) {
		t.Errorf("argv mismatch: got %v, want %v", segs[0].argv, expectedArgv)
	}

	// Test nested parens inside substitution
	lineNested := `echo $(echo (nested))`
	_, subsNested, err := parseShell(lineNested)
	if err != nil {
		t.Fatalf("unexpected error on nested paren: %v", err)
	}
	if len(subsNested) != 1 || subsNested[0] != "echo (nested)" {
		t.Errorf("expected sub 'echo (nested)', got %v", subsNested)
	}

	// Test unterminated command substitution
	_, _, err = parseShell(`echo $(date`)
	if err == nil || !strings.Contains(err.Error(), "unterminated command substitution") {
		t.Fatalf("expected unterminated command substitution error, got %v", err)
	}
}

func TestParseShellBacktickSubstitution(t *testing.T) {
	line := "echo `hostname` \"host: `uname -a`\""
	segs, subs, err := parseShell(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedSubs := []string{"hostname", "uname -a"}
	if !reflect.DeepEqual(subs, expectedSubs) {
		t.Errorf("subs mismatch: got %v, want %v", subs, expectedSubs)
	}

	if len(segs) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segs))
	}
	expectedArgv := []string{"echo", "$SUBST", "host: $SUBST"}
	if !reflect.DeepEqual(segs[0].argv, expectedArgv) {
		t.Errorf("argv mismatch: got %v, want %v", segs[0].argv, expectedArgv)
	}

	// Unterminated backtick
	_, _, err = parseShell("echo `hostname")
	if err == nil || !strings.Contains(err.Error(), "unterminated backtick substitution") {
		t.Fatalf("expected unterminated backtick substitution error, got %v", err)
	}
}

func TestParseShellProcessSubstitution(t *testing.T) {
	line := "diff <(sort file1) >(sort file2)"
	segs, subs, err := parseShell(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedSubs := []string{"sort file1", "sort file2"}
	if !reflect.DeepEqual(subs, expectedSubs) {
		t.Errorf("subs mismatch: got %v, want %v", subs, expectedSubs)
	}

	if len(segs) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segs))
	}
	expectedArgv := []string{"diff", "$SUBST", "$SUBST"}
	if !reflect.DeepEqual(segs[0].argv, expectedArgv) {
		t.Errorf("argv mismatch: got %v, want %v", segs[0].argv, expectedArgv)
	}
}

func TestParseShellRedirections(t *testing.T) {
	tests := []struct {
		name          string
		line          string
		wantArgv      []string
		wantRedirects []*redirect
	}{
		{
			name:     "output redirect",
			line:     "echo hello > out.txt",
			wantArgv: []string{"echo", "hello"},
			wantRedirects: []*redirect{
				{op: ">", target: "out.txt"},
			},
		},
		{
			name:     "append and input redirect",
			line:     "cat < input.txt >> out.log",
			wantArgv: []string{"cat"},
			wantRedirects: []*redirect{
				{op: "<", target: "input.txt"},
				{op: ">>", target: "out.log"},
			},
		},
		{
			name:     "fd redirect stderr",
			line:     "go test 2> errors.txt",
			wantArgv: []string{"go", "test"},
			wantRedirects: []*redirect{
				{op: ">", target: "errors.txt", fd: "2"},
			},
		},
		{
			name:     "all-output redirect ampersand",
			line:     "build_cmd &> build.log",
			wantArgv: []string{"build_cmd"},
			wantRedirects: []*redirect{
				{op: "&>", target: "build.log"},
			},
		},
		{
			name:     "all-output append redirect",
			line:     "build_cmd &>> build.log",
			wantArgv: []string{"build_cmd"},
			wantRedirects: []*redirect{
				{op: "&>>", target: "build.log"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			segs, _, err := parseShell(tt.line)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(segs) != 1 {
				t.Fatalf("expected 1 segment, got %d", len(segs))
			}
			if !reflect.DeepEqual(segs[0].argv, tt.wantArgv) {
				t.Errorf("argv mismatch: got %v, want %v", segs[0].argv, tt.wantArgv)
			}
			if !reflect.DeepEqual(segs[0].redirects, tt.wantRedirects) {
				t.Errorf("redirects mismatch: got %+v, want %+v", segs[0].redirects, tt.wantRedirects)
			}
		})
	}
}

func TestParseShellPipesAndSeparators(t *testing.T) {
	t.Run("pipeline", func(t *testing.T) {
		line := "cat access.log | grep 404 | wc -l"
		segs, _, err := parseShell(line)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(segs) != 3 {
			t.Fatalf("expected 3 segments, got %d", len(segs))
		}
		if !segs[0].piped {
			t.Errorf("expected seg 0 to be piped")
		}
		if !segs[1].piped {
			t.Errorf("expected seg 1 to be piped")
		}
		if segs[2].piped {
			t.Errorf("expected seg 2 not to be piped")
		}
		if !reflect.DeepEqual(segs[0].argv, []string{"cat", "access.log"}) {
			t.Errorf("seg 0 mismatch: %v", segs[0].argv)
		}
		if !reflect.DeepEqual(segs[1].argv, []string{"grep", "404"}) {
			t.Errorf("seg 1 mismatch: %v", segs[1].argv)
		}
		if !reflect.DeepEqual(segs[2].argv, []string{"wc", "-l"}) {
			t.Errorf("seg 2 mismatch: %v", segs[2].argv)
		}
	})

	t.Run("semicolon and logical operators", func(t *testing.T) {
		line := "cmd1 arg1; cmd2 arg2 && cmd3 arg3 || cmd4 arg4"
		segs, _, err := parseShell(line)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(segs) != 4 {
			t.Fatalf("expected 4 segments, got %d", len(segs))
		}
		for i, seg := range segs {
			if seg.piped {
				t.Errorf("segment %d should not be marked piped", i)
			}
		}
	})

	t.Run("newline and parentheses", func(t *testing.T) {
		line := "(cmd1)\ncmd2"
		segs, _, err := parseShell(line)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(segs) != 2 {
			t.Fatalf("expected 2 segments, got %d", len(segs))
		}
		if !reflect.DeepEqual(segs[0].argv, []string{"cmd1"}) {
			t.Errorf("seg 0 mismatch: %v", segs[0].argv)
		}
		if !reflect.DeepEqual(segs[1].argv, []string{"cmd2"}) {
			t.Errorf("seg 1 mismatch: %v", segs[1].argv)
		}
	})
}

func TestParseShellHelpers(t *testing.T) {
	t.Run("isDigits", func(t *testing.T) {
		if isDigits("") {
			t.Errorf("empty string should not be digits")
		}
		if !isDigits("0") || !isDigits("1234567890") {
			t.Errorf("numeric string should be digits")
		}
		if isDigits("123a") || isDigits("a123") || isDigits(" ") {
			t.Errorf("non-numeric strings should not be digits")
		}
	})

	t.Run("scanParen", func(t *testing.T) {
		input := []rune("((a + b) * c)")
		body, end, err := scanParen(input, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if body != "(a + b) * c" {
			t.Errorf("expected '(a + b) * c', got %q", body)
		}
		if end != len(input)-1 {
			t.Errorf("expected end at %d, got %d", len(input)-1, end)
		}

		// Unterminated paren
		_, _, err = scanParen([]rune("(unclosed"), 0)
		if err == nil {
			t.Errorf("expected error for unclosed paren")
		}
	})

	t.Run("scanBacktick", func(t *testing.T) {
		input := []rune("`hello \\`world\\``")
		body, end, err := scanBacktick(input, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if body != "hello \\`world\\`" {
			t.Errorf("unexpected body: %q", body)
		}
		if end != len(input)-1 {
			t.Errorf("expected end at %d, got %d", len(input)-1, end)
		}

		// Unterminated backtick
		_, _, err = scanBacktick([]rune("`unclosed"), 0)
		if err == nil {
			t.Errorf("expected error for unclosed backtick")
		}
	})
}
