package board

import (
	"fmt"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/charmbracelet/lipgloss"
)

// renderThreadView renders the full detail and thread view of the selected task.
func (m *Model) renderThreadView() string {
	task := m.activeTask
	if task == nil {
		return "No task selected. Press [Esc] to return to board."
	}

	var b strings.Builder

	// Top Bar: Back & Action keys
	backBtn := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("15")).
		Background(lipgloss.Color("63")).
		Padding(0, 1).
		Render("◄ Back [Esc]")

	actions := helpBarStyle.Render("  [c: Add Comment]  [m: Change Stage]  [b: Toggle Block]  [j/k: Scroll]  [q: Back]")
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Center, backBtn, actions))
	b.WriteString("\n\n")

	if m.confirmBlock {
		verb := "Block"
		if task.IsBlocked {
			verb = "Unblock"
		}
		b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(colYellow).
			Render(fmt.Sprintf("%s this task? [y] yes  [any other key] cancel", verb)))
		b.WriteString("\n\n")
	} else if m.statusMessage != "" {
		color := colTextMuted
		if m.statusIsErr {
			color = colRed
		}
		b.WriteString(lipgloss.NewStyle().Foreground(color).Render(m.statusMessage))
		b.WriteString("\n\n")
	}

	// Task Header Card
	stageColor := columnHeaderColor(MapTaskToColumn(*task))
	stageBadge := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("0")).
		Background(stageColor).
		Padding(0, 1).
		Render(strings.ToUpper(task.ExecutionStage))

	blockedBadge := ""
	if task.IsBlocked {
		blockedBadge = " " + lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15")).
			Background(colRed).
			Padding(0, 1).
			Render("BLOCKED: "+task.BlockReason)
	}

	titleLine := fmt.Sprintf("%s  %s%s", lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("255")).Render(task.Name), stageBadge, blockedBadge)

	metaGrid := []string{
		fmt.Sprintf("ID: %s", task.ID),
		fmt.Sprintf("Branch: %s", task.GitBranch),
		fmt.Sprintf("Repo: %s", task.RepoPath),
		fmt.Sprintf("Role: %s", task.AccountRole),
		fmt.Sprintf("Spend: $%.2f / $%.2f", task.SpentUSD, task.MaxBudgetUSD),
		fmt.Sprintf("Turns: %d / %d", task.SpentTurns, task.MaxTurns),
		fmt.Sprintf("Created: %s", task.CreatedAt),
		fmt.Sprintf("Updated: %s", task.UpdatedAt),
	}

	if task.CheckoutAgentID != "" || task.CheckoutRunID != "" {
		metaGrid = append(metaGrid, fmt.Sprintf("Runner: agent=%s run=%s", task.CheckoutAgentID, task.CheckoutRunID))
	}

	metaBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colBorderActive).
		Padding(0, 1).
		Width(m.width - 4).
		Render(titleLine + "\n\n" + strings.Join(metaGrid, "  ·  "))

	b.WriteString(metaBox)
	b.WriteString("\n\n")

	// Thread & Feed Content inside Viewport
	b.WriteString(m.viewport.View())

	return b.String()
}

// buildThreadContent formats the complete scrollable content for the viewport.
func buildThreadContent(task *context.Task, comments []context.TaskComment, products []context.TaskWorkProduct, activity []context.ActivityLog, width int) string {
	var sb strings.Builder

	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(colCyan).Underline(true)

	// 1. Work Products
	if len(products) > 0 {
		sb.WriteString(sectionStyle.Render("Deliverables & Work Products"))
		sb.WriteString("\n")
		for _, wp := range products {
			prodBadge := lipgloss.NewStyle().Foreground(colYellow).Render(fmt.Sprintf("[%s]", wp.ProductType))
			sb.WriteString(fmt.Sprintf("  • %s %s  (%s)\n", prodBadge, wp.Reference, wp.CreatedAt))
		}
		sb.WriteString("\n")
	}

	// 2. Comments Thread
	sb.WriteString(sectionStyle.Render(fmt.Sprintf("Discussion Thread (%d comments)", len(comments))))
	sb.WriteString("\n")

	if len(comments) == 0 {
		sb.WriteString(lipgloss.NewStyle().Foreground(colTextMuted).Italic(true).Render("  No comments yet. Press 'c' to add the first comment."))
		sb.WriteString("\n\n")
	} else {
		for _, c := range comments {
			authorColor := colGreen
			if strings.EqualFold(c.Author, "user") {
				authorColor = colCyan
			} else if strings.EqualFold(c.Author, "system") {
				authorColor = colTextMuted
			}

			authorBadge := lipgloss.NewStyle().Bold(true).Foreground(authorColor).Render(fmt.Sprintf("[%s]", c.Author))
			ts := lipgloss.NewStyle().Foreground(colTextMuted).Render(c.CreatedAt)

			sb.WriteString(fmt.Sprintf("%s %s\n", authorBadge, ts))

			// Format message lines
			msgBox := lipgloss.NewStyle().
				Border(lipgloss.NormalBorder(), false, false, false, true).
				BorderForeground(colBorderInactive).
				PaddingLeft(1).
				Width(width - 6).
				Render(c.Message)

			sb.WriteString(msgBox)
			sb.WriteString("\n\n")
		}
	}

	// 3. Activity Log
	if len(activity) > 0 {
		sb.WriteString(sectionStyle.Render(fmt.Sprintf("Activity Log (%d entries)", len(activity))))
		sb.WriteString("\n")
		for _, act := range activity {
			evtBadge := lipgloss.NewStyle().Foreground(colPurple).Render(fmt.Sprintf("[%s]", act.EventType))
			ts := lipgloss.NewStyle().Foreground(colTextMuted).Render(act.CreatedAt)
			sb.WriteString(fmt.Sprintf("  %s %s  %s\n", evtBadge, ts, act.Details))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}
