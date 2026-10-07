package board

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/charmbracelet/lipgloss"
)

var (
	// Colors
	colBorderInactive = lipgloss.Color("238")
	colBorderActive   = lipgloss.Color("39")
	colCardBorder     = lipgloss.Color("237")
	colCardSelected   = lipgloss.Color("75")
	colTextMuted      = lipgloss.Color("243")
	colGreen          = lipgloss.Color("42")
	colYellow         = lipgloss.Color("214")
	colRed            = lipgloss.Color("196")
	colPurple         = lipgloss.Color("141")
	colCyan           = lipgloss.Color("51")

	// Styles
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15")).
			Background(lipgloss.Color("57")).
			Padding(0, 1)

	subHeaderStyle = lipgloss.NewStyle().
			Foreground(colTextMuted).
			Padding(0, 1)

	helpBarStyle = lipgloss.NewStyle().
			Foreground(colTextMuted).
			Padding(0, 1)

	statusOkStyle = lipgloss.NewStyle().
			Foreground(colGreen).
			Bold(true)

	statusWarnStyle = lipgloss.NewStyle().
			Foreground(colYellow)

	statusErrStyle = lipgloss.NewStyle().
			Foreground(colRed).
			Bold(true)
)

func columnHeaderColor(col ColumnType) lipgloss.Color {
	switch col {
	case ColBacklog:
		return lipgloss.Color("245")
	case ColTodo:
		return colCyan
	case ColInProgress:
		return colYellow
	case ColInReview:
		return colPurple
	case ColDone:
		return colGreen
	default:
		return lipgloss.Color("255")
	}
}

// renderBoardView renders the complete 4-column Kanban board view.
func (m *Model) renderBoardView() string {
	var b strings.Builder

	// 1. Top Header Bar
	headerLeft := headerStyle.Render(" STAYPOINT BOARD ")
	dbName := filepath.Base(m.cfg.DBPath)
	if dbName == "." || dbName == "/" {
		dbName = "staypoint.db"
	}

	daemonStatus := statusWarnStyle.Render("[DAEMON: STANDALONE]")
	if m.daemonConnected {
		daemonStatus = statusOkStyle.Render("[DAEMON: CONNECTED (SSE)]")
	}

	totalTasks := len(m.allTasks)
	metaText := subHeaderStyle.Render(fmt.Sprintf("DB: %s  ·  %s  ·  Tasks: %d", dbName, daemonStatus, totalTasks))
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Center, headerLeft, metaText))
	b.WriteString("\n")

	// 2. Compute Column Dimensions
	numCols := len(AllColumns)
	colWidth := (m.width - (numCols * 2) - 2) / numCols
	if colWidth < 22 {
		colWidth = 22
	}
	colHeight := m.height - 6
	if colHeight < 10 {
		colHeight = 10
	}

	// 3. Render Each Column
	renderedCols := make([]string, numCols)
	for i, colType := range AllColumns {
		isActiveCol := (i == m.activeCol)
		renderedCols[i] = m.renderColumn(colType, i, isActiveCol, colWidth, colHeight)
	}

	boardRow := lipgloss.JoinHorizontal(lipgloss.Top, renderedCols...)
	b.WriteString(boardRow)
	b.WriteString("\n")

	// 4. Status or Error Banner
	if m.statusMessage != "" {
		stStyle := statusOkStyle
		if m.statusIsErr {
			stStyle = statusErrStyle
		}
		b.WriteString(stStyle.Render("  " + m.statusMessage))
		b.WriteString("\n")
	} else {
		// 5. Help Hint Bar
		helpText := helpBarStyle.Render("←/→/h/l: Columns  ·  ↑/↓/j/k: Tasks  ·  Enter: Thread View  ·  m: Move Task  ·  c: Comment  ·  r: Refresh  ·  q: Quit")
		b.WriteString(helpText)
		b.WriteString("\n")
	}

	return b.String()
}

// renderColumn renders a single Kanban column with its cards.
func (m *Model) renderColumn(col ColumnType, colIdx int, isActive bool, width, height int) string {
	tasks := m.columns[col]
	count := len(tasks)

	// Column title bar
	titleColor := columnHeaderColor(col)
	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(titleColor).
		Padding(0, 1)

	titleStr := fmt.Sprintf("%s (%d)", ColumnTitle(col), count)
	if isActive {
		titleStr = "▶ " + titleStr
	}
	renderedTitle := titleStyle.Render(titleStr)

	// Column content area
	var cardViews []string
	if count == 0 {
		emptyStyle := lipgloss.NewStyle().
			Foreground(colTextMuted).
			Italic(true).
			Padding(1, 2)
		cardViews = append(cardViews, emptyStyle.Render("No tasks"))
	} else {
		cursorIdx := m.columnCursor[colIdx]
		if cursorIdx >= count {
			cursorIdx = count - 1
			m.columnCursor[colIdx] = cursorIdx
		}

		// Calculate scrolling window so active card is always visible
		visibleCards := (height - 3) / 5
		if visibleCards < 2 {
			visibleCards = 2
		}

		startIdx := 0
		if cursorIdx >= visibleCards {
			startIdx = cursorIdx - visibleCards + 1
		}
		endIdx := startIdx + visibleCards
		if endIdx > count {
			endIdx = count
		}

		for idx := startIdx; idx < endIdx; idx++ {
			t := tasks[idx]
			isSelected := isActive && (idx == cursorIdx)
			cardViews = append(cardViews, renderTaskCard(t, isSelected, width-2))
		}
	}

	content := strings.Join(cardViews, "\n")

	// Column container frame
	borderCol := colBorderInactive
	if isActive {
		borderCol = colBorderActive
	}

	colBoxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderCol).
		Width(width).
		Height(height).
		Margin(0, 1)

	return colBoxStyle.Render(renderedTitle + "\n" + content)
}

// renderTaskCard renders an individual card in a column.
func renderTaskCard(t context.Task, isSelected bool, width int) string {
	innerWidth := width - 4
	if innerWidth < 16 {
		innerWidth = 16
	}

	// Title line (truncated)
	title := t.Name
	if len(title) > innerWidth {
		title = title[:innerWidth-3] + "..."
	}

	titleStyle := lipgloss.NewStyle().Bold(isSelected)
	if isSelected {
		titleStyle = titleStyle.Foreground(lipgloss.Color("255"))
	} else {
		titleStyle = titleStyle.Foreground(lipgloss.Color("250"))
	}

	// Short ID and status badges
	shortID := t.ID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}

	var badges []string
	badges = append(badges, fmt.Sprintf("#%s", shortID))

	if t.IsBlocked {
		badges = append(badges, lipgloss.NewStyle().Foreground(colRed).Bold(true).Render("[BLOCKED]"))
	}

	if t.CheckoutAgentID != "" || t.CheckoutRunID != "" {
		badges = append(badges, lipgloss.NewStyle().Foreground(colGreen).Render("● running"))
	}

	// Git branch & spend
	metaParts := []string{}
	if t.GitBranch != "" {
		branch := t.GitBranch
		if len(branch) > 12 {
			branch = branch[:10] + ".."
		}
		metaParts = append(metaParts, branch)
	}

	if t.SpentUSD > 0 || t.SpentTurns > 0 {
		metaParts = append(metaParts, fmt.Sprintf("$%.2f/%dt", t.SpentUSD, t.SpentTurns))
	}

	badgeLine := strings.Join(badges, " ")
	metaLine := lipgloss.NewStyle().Foreground(colTextMuted).Render(strings.Join(metaParts, " · "))

	cardContent := fmt.Sprintf("%s\n%s", titleStyle.Render(title), badgeLine)
	if metaLine != "" {
		cardContent += "\n" + metaLine
	}

	borderCol := colCardBorder
	if isSelected {
		borderCol = colCardSelected
	}

	cardStyle := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(borderCol).
		Width(width - 2).
		Padding(0, 1)

	return cardStyle.Render(cardContent)
}
