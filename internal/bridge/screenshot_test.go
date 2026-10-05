package bridge

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuoteForAppleScript(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			input: "/tmp/screenshot.png",
			want:  `"/tmp/screenshot.png"`,
		},
		{
			input: `/path/with "quotes" and \slashes\`,
			want:  `"/path/with \"quotes\" and \\slashes\\"`,
		},
	}

	for _, tc := range tests {
		got := quoteForAppleScript(tc.input)
		if got != tc.want {
			t.Errorf("quoteForAppleScript(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestFindRecentDesktopScreenshot(t *testing.T) {
	tmpDir := t.TempDir()
	// The test creates ~/Desktop, so keep it out of the real home (STA-741).
	t.Setenv("HOME", t.TempDir())
	home := getHomeDir()
	desktopDir := filepath.Join(home, "Desktop")
	_ = os.MkdirAll(desktopDir, 0755)

	// Create an old file
	oldFile := filepath.Join(tmpDir, "Screenshot_old.png")
	_ = os.WriteFile(oldFile, []byte("fake png data"), 0644)
	oldTime := time.Now().Add(-10 * time.Minute)
	_ = os.Chtimes(oldFile, oldTime, oldTime)

	// Verify discovery doesn't crash on standard scanning
	_ = findRecentDesktopScreenshot()
}
