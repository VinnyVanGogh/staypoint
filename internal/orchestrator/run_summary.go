package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// extractFinalResponse parses the last assistant text block from adapter stream-json output.
// It handles both Claude stream-json ("type":"assistant") and Agy stream-json ("event":"result").
// The [[TASK_COMPLETE]] marker is stripped from the returned text.
func extractFinalResponse(data []byte) string {
	var lastAssistantText string
	var agyResultText string

	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}

		// Claude format: type=="assistant" with message.content[].text
		var claudeEv struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &claudeEv) == nil && claudeEv.Type == "assistant" {
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(claudeEv.Message.Content, &blocks) == nil {
				var sb strings.Builder
				for _, b := range blocks {
					if b.Type == "text" {
						sb.WriteString(b.Text)
					}
				}
				if t := sb.String(); t != "" {
					lastAssistantText = t
				}
			}
		}

		// Agy format: event=="result" with result.response
		var agyEv struct {
			Event  string `json:"event"`
			Result struct {
				Response string `json:"response"`
			} `json:"result"`
		}
		if json.Unmarshal(line, &agyEv) == nil && agyEv.Event == "result" && agyEv.Result.Response != "" {
			agyResultText = agyEv.Result.Response
		}
	}

	text := lastAssistantText
	if text == "" {
		text = agyResultText
	}
	text = strings.ReplaceAll(text, taskCompleteMarker, "")
	return strings.TrimSpace(text)
}

// buildRunFooter assembles the markdown footer appended to the agent's run-summary comment.
func buildRunFooter(result *RunResult, wtPath string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var sb strings.Builder
	sb.WriteString("---\n**Run summary**\n\n")

	sb.WriteString(fmt.Sprintf("- **Disposition:** `%s`\n", result.Disposition))
	sb.WriteString(fmt.Sprintf("- **Turns:** %d\n", result.Turns))

	if result.DiffStat != "" {
		sb.WriteString(fmt.Sprintf("- **Files changed:** %s\n", strings.TrimSpace(result.DiffStat)))
	}

	if branch := gitCurrentBranch(ctx, wtPath); branch != "" {
		sb.WriteString(fmt.Sprintf("- **Branch:** `%s`\n", branch))
	}
	if sha := gitHeadSHA(ctx, wtPath); sha != "" {
		sb.WriteString(fmt.Sprintf("- **Head SHA:** `%s`\n", sha))
	}

	if flags := detectNeedsAttention(result.DiffStat); len(flags) > 0 {
		sb.WriteString("\n**Needs attention:**\n")
		for _, f := range flags {
			sb.WriteString(fmt.Sprintf("- ⚠️ %s\n", f))
		}
	}

	if result.Disposition == "capped" {
		sb.WriteString("\n> **Note:** Run stopped early (turn/wall-clock cap reached).\n")
	}

	return sb.String()
}

// detectNeedsAttention scans a git diff --stat string for files that warrant a human look.
func detectNeedsAttention(diffStat string) []string {
	if diffStat == "" {
		return nil
	}
	lower := strings.ToLower(diffStat)
	var flags []string
	if strings.Contains(lower, "migration") || strings.Contains(lower, "migrate") ||
		(strings.Contains(lower, ".sql") && !strings.Contains(lower, "_test.sql")) {
		flags = append(flags, "DB migration files changed")
	}
	depFiles := []string{"go.mod", "go.sum", "package.json", "package-lock.json",
		"yarn.lock", "gemfile", "requirements.txt", "pyproject.toml"}
	for _, dep := range depFiles {
		if strings.Contains(lower, dep) {
			flags = append(flags, "dependency files changed (`"+dep+"`)")
			break
		}
	}
	if strings.Contains(lower, ".env") {
		flags = append(flags, "`.env*` file touched")
	}
	return flags
}

// gitCurrentBranch returns the current branch name in wtPath, or "" on error.
func gitCurrentBranch(ctx context.Context, wtPath string) string {
	if wtPath == "" {
		return ""
	}
	cmd := gitexec.Command(ctx, "branch", "--show-current")
	cmd.Dir = wtPath
	cmd.Env = security.ChildEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitHeadSHA returns the short HEAD commit SHA in wtPath, or "" on error.
func gitHeadSHA(ctx context.Context, wtPath string) string {
	if wtPath == "" {
		return ""
	}
	cmd := gitexec.Command(ctx, "rev-parse", "--short", "HEAD")
	cmd.Dir = wtPath
	cmd.Env = security.ChildEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
