package main

import (
	"os"
	"strings"
)

func main() {
	b, _ := os.ReadFile("internal/server/webui/app.js")
	s := string(b)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.Contains(line, "if (payloadObj.ai_suggestion) {n      const") {
			lines[i] = `
    if (payloadObj.ai_suggestion) {
      const suggestionBox = el('div', 'interaction-ai-suggestion');
      suggestionBox.style.cssText = 'margin-top: 12px; padding: 12px; background: rgba(56, 189, 248, 0.1); border-left: 3px solid var(--accent, #38bdf8); border-radius: 4px; font-size: 0.85rem; color: var(--text-muted);';
      const title = el('div', '', '');
      title.innerHTML='<strong>AI Suggestion:</strong>';
      suggestionBox.appendChild(title);
      suggestionBox.appendChild(mdEl(payloadObj.ai_suggestion));
      body.appendChild(suggestionBox);
    }
`
		}
	}
	os.WriteFile("internal/server/webui/app.js", []byte(strings.Join(lines, "\n")), 0644)
}
