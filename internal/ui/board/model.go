package board

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	_ "modernc.org/sqlite"
)

// Model represents the state of the StayPoint Board TUI.
type Model struct {
	cfg       Config
	db        *sql.DB
	ownsDB    bool
	sseClient SSEClientInterface

	viewMode ViewMode
	prevMode ViewMode

	allTasks     []meshContext.Task
	columns      map[ColumnType][]meshContext.Task
	columnCursor [len(AllColumns)]int
	activeCol    int

	activeTask     *meshContext.Task
	activeComments []meshContext.TaskComment
	activeProducts []meshContext.TaskWorkProduct
	activeActivity []meshContext.ActivityLog

	// confirmBlock is set after 'b' in the thread view; the toggle runs only
	// on 'y', so a stray keypress can't silently block a task.
	confirmBlock bool

	viewport viewport.Model
	textarea textarea.Model

	eventsChan chan DaemonEvent
	statusChan chan daemonStatusMsg

	daemonConnected bool
	statusMessage   string
	statusIsErr     bool

	width    int
	height   int
	quitting bool
}

// NewModel initializes the Board TUI model.
func NewModel(cfg Config) (*Model, error) {
	var database *sql.DB
	ownsDB := false

	if cfg.CustomDB != nil {
		database = cfg.CustomDB
	} else if cfg.DBPath != "" {
		dbStore, err := db.Open(cfg.DBPath)
		if err != nil {
			return nil, fmt.Errorf("failed to open database: %w", err)
		}
		database = dbStore.DB()
		ownsDB = true
	} else {
		return nil, fmt.Errorf("database path or connection is required")
	}

	ta := textarea.New()
	ta.Placeholder = "Write a comment... (Ctrl+S or Enter to submit, Esc to cancel)"
	ta.CharLimit = 4000
	ta.SetWidth(70)
	ta.SetHeight(5)

	vp := viewport.New(80, 20)

	cols := make(map[ColumnType][]meshContext.Task)
	for _, c := range AllColumns {
		cols[c] = []meshContext.Task{}
	}

	var sseClient SSEClientInterface
	if !cfg.Standalone {
		if cfg.CustomSSEClient != nil {
			sseClient = cfg.CustomSSEClient
		} else {
			sseClient = NewSSEClient(cfg.DaemonURL, cfg.Token, cfg.HTTPClient)
		}
	}

	m := &Model{
		cfg:        cfg,
		db:         database,
		ownsDB:     ownsDB,
		sseClient:  sseClient,
		viewMode:   ViewBoard,
		columns:    cols,
		activeCol:  1, // todo; backlog (0) is parked work
		textarea:   ta,
		viewport:   vp,
		eventsChan: make(chan DaemonEvent, 32),
		statusChan: make(chan daemonStatusMsg, 8),
		width:      100,
		height:     30,
	}

	return m, nil
}

// Init starts initial task loading and SSE streaming.
func (m *Model) Init() tea.Cmd {
	var cmds []tea.Cmd
	cmds = append(cmds, m.loadTasksCmd())

	if m.sseClient != nil {
		m.sseClient.Start(context.Background(), m.eventsChan, m.statusChan)
		cmds = append(cmds, m.waitForSSEEventCmd(), m.waitForSSEStatusCmd())
	}

	return tea.Batch(cmds...)
}

// Update processes Bubble Tea events and key messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		keyStr := msg.String()
		lowerKey := strings.ToLower(keyStr)

		// Global quit
		if keyStr == "ctrl+c" {
			m.quitting = true
			if m.ownsDB && m.db != nil {
				_ = m.db.Close()
			}
			if m.sseClient != nil {
				_ = m.sseClient.Close()
			}
			return m, tea.Quit
		}

		switch m.viewMode {
		case ViewBoard:
			switch lowerKey {
			case "q":
				m.quitting = true
				if m.ownsDB && m.db != nil {
					_ = m.db.Close()
				}
				if m.sseClient != nil {
					_ = m.sseClient.Close()
				}
				return m, tea.Quit
			case "h", "left":
				m.activeCol = (m.activeCol - 1 + len(AllColumns)) % len(AllColumns)
			case "l", "right":
				m.activeCol = (m.activeCol + 1) % len(AllColumns)
			case "k", "up":
				if m.columnCursor[m.activeCol] > 0 {
					m.columnCursor[m.activeCol]--
				}
			case "j", "down":
				colType := AllColumns[m.activeCol]
				tasks := m.columns[colType]
				if m.columnCursor[m.activeCol] < len(tasks)-1 {
					m.columnCursor[m.activeCol]++
				}
			case "enter", " ", "t":
				colType := AllColumns[m.activeCol]
				tasks := m.columns[colType]
				if len(tasks) > 0 && m.columnCursor[m.activeCol] < len(tasks) {
					task := tasks[m.columnCursor[m.activeCol]]
					m.activeTask = &task
					m.viewMode = ViewThread
					cmds = append(cmds, m.loadThreadCmd(task.ID))
				}
			case "m":
				colType := AllColumns[m.activeCol]
				tasks := m.columns[colType]
				if len(tasks) > 0 && m.columnCursor[m.activeCol] < len(tasks) {
					task := tasks[m.columnCursor[m.activeCol]]
					m.activeTask = &task
					m.prevMode = ViewBoard
					m.viewMode = ViewMove
				}
			case "c":
				colType := AllColumns[m.activeCol]
				tasks := m.columns[colType]
				if len(tasks) > 0 && m.columnCursor[m.activeCol] < len(tasks) {
					task := tasks[m.columnCursor[m.activeCol]]
					m.activeTask = &task
					m.prevMode = ViewBoard
					m.viewMode = ViewCommentInput
					m.textarea.Reset()
					m.textarea.Focus()
				}
			case "r":
				m.statusMessage = "Refreshed from DB"
				m.statusIsErr = false
				cmds = append(cmds, m.loadTasksCmd())
			case "?":
				m.prevMode = ViewBoard
				m.viewMode = ViewHelp
			}

		case ViewThread:
			if m.confirmBlock {
				m.confirmBlock = false
				if lowerKey == "y" && m.activeTask != nil {
					cmds = append(cmds, m.toggleBlockCmd(m.activeTask))
				} else {
					m.statusMessage = "Block toggle cancelled"
					m.statusIsErr = false
				}
				break
			}
			switch lowerKey {
			case "esc", "q", "backspace":
				m.viewMode = ViewBoard
			case "c":
				m.prevMode = ViewThread
				m.viewMode = ViewCommentInput
				m.textarea.Reset()
				m.textarea.Focus()
			case "m":
				m.prevMode = ViewThread
				m.viewMode = ViewMove
			case "b":
				if m.activeTask != nil {
					m.confirmBlock = true
				}
			default:
				var vpCmd tea.Cmd
				m.viewport, vpCmd = m.viewport.Update(msg)
				if vpCmd != nil {
					cmds = append(cmds, vpCmd)
				}
			}

		case ViewMove:
			switch lowerKey {
			case "esc", "q":
				m.viewMode = m.prevMode
			case "0", "b":
				if m.activeTask != nil {
					cmds = append(cmds, m.changeStageCmd(m.activeTask.ID, string(ColBacklog)))
				}
				m.viewMode = m.prevMode
			case "1", "t":
				if m.activeTask != nil {
					cmds = append(cmds, m.changeStageCmd(m.activeTask.ID, string(ColTodo)))
				}
				m.viewMode = m.prevMode
			case "2", "p":
				if m.activeTask != nil {
					cmds = append(cmds, m.changeStageCmd(m.activeTask.ID, string(ColInProgress)))
				}
				m.viewMode = m.prevMode
			case "3", "r":
				if m.activeTask != nil {
					cmds = append(cmds, m.changeStageCmd(m.activeTask.ID, string(ColInReview)))
				}
				m.viewMode = m.prevMode
			case "4", "d":
				if m.activeTask != nil {
					cmds = append(cmds, m.changeStageCmd(m.activeTask.ID, string(ColDone)))
				}
				m.viewMode = m.prevMode
			}

		case ViewCommentInput:
			switch lowerKey {
			case "esc":
				m.viewMode = m.prevMode
				m.textarea.Blur()
			case "enter", "ctrl+s":
				text := strings.TrimSpace(m.textarea.Value())
				if text != "" && m.activeTask != nil {
					cmds = append(cmds, m.addCommentCmd(m.activeTask.ID, text))
				}
				m.viewMode = m.prevMode
				m.textarea.Blur()
			default:
				var taCmd tea.Cmd
				m.textarea, taCmd = m.textarea.Update(msg)
				if taCmd != nil {
					cmds = append(cmds, taCmd)
				}
			}

		case ViewHelp:
			switch lowerKey {
			case "esc", "q", "enter", "?":
				m.viewMode = m.prevMode
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.viewport.Width = msg.Width - 6
		m.viewport.Height = msg.Height - 12
		m.textarea.SetWidth(msg.Width - 10)

	case tasksLoadedMsg:
		if msg.err != nil {
			m.statusMessage = "DB load error: " + msg.err.Error()
			m.statusIsErr = true
		} else {
			m.allTasks = msg.tasks
			m.rebuildColumns()
			// Update activeTask if currently selected
			if m.activeTask != nil {
				for _, t := range m.allTasks {
					if t.ID == m.activeTask.ID {
						updated := t
						m.activeTask = &updated
						break
					}
				}
			}
		}

	case threadLoadedMsg:
		if msg.err == nil && m.activeTask != nil && m.activeTask.ID == msg.taskID {
			m.activeComments = msg.comments
			m.activeProducts = msg.workProducts
			m.activeActivity = msg.activity
			content := buildThreadContent(m.activeTask, m.activeComments, m.activeProducts, m.activeActivity, m.width)
			m.viewport.SetContent(content)
		}

	case daemonEventMsg:
		// Live event from daemon SSE!
		m.daemonConnected = true
		// Reactive update: refresh task columns and thread from DB to match state
		cmds = append(cmds, m.loadTasksCmd())
		if m.activeTask != nil {
			cmds = append(cmds, m.loadThreadCmd(m.activeTask.ID))
		}
		// Continue listening for next SSE event
		cmds = append(cmds, m.waitForSSEEventCmd())

	case daemonStatusMsg:
		m.daemonConnected = msg.connected
		// Continue listening for next status update
		cmds = append(cmds, m.waitForSSEStatusCmd())

	case stageChangedMsg:
		if msg.err != nil {
			m.statusMessage = "Failed to change stage: " + msg.err.Error()
			m.statusIsErr = true
		} else {
			m.statusMessage = fmt.Sprintf("Task stage changed to %s", msg.stage)
			m.statusIsErr = false
			cmds = append(cmds, m.loadTasksCmd())
			if m.activeTask != nil && m.activeTask.ID == msg.taskID {
				cmds = append(cmds, m.loadThreadCmd(msg.taskID))
			}
		}

	case commentAddedMsg:
		if msg.err != nil {
			m.statusMessage = "Failed to add comment: " + msg.err.Error()
			m.statusIsErr = true
		} else {
			m.statusMessage = "Comment added"
			m.statusIsErr = false
			cmds = append(cmds, m.loadTasksCmd())
			if m.activeTask != nil && m.activeTask.ID == msg.taskID {
				cmds = append(cmds, m.loadThreadCmd(msg.taskID))
			}
		}

	case blockToggledMsg:
		if msg.err != nil {
			m.statusMessage = "Block toggle failed: " + msg.err.Error()
			m.statusIsErr = true
		} else {
			m.statusMessage = "Task unblocked"
			if msg.blocked {
				m.statusMessage = "Task blocked"
			}
			m.statusIsErr = false
			cmds = append(cmds, m.loadTasksCmd())
			if m.activeTask != nil && m.activeTask.ID == msg.taskID {
				cmds = append(cmds, m.loadThreadCmd(msg.taskID))
			}
		}

	case statusMessageMsg:
		m.statusMessage = msg.message
		m.statusIsErr = msg.isError
	}

	return m, tea.Batch(cmds...)
}

// View renders the TUI according to viewMode.
func (m *Model) View() string {
	if m.quitting {
		return ""
	}

	switch m.viewMode {
	case ViewThread:
		return m.renderThreadView()
	case ViewCommentInput:
		return m.renderCommentInputView()
	case ViewMove:
		return m.renderMoveModalView()
	case ViewHelp:
		return m.renderHelpView()
	case ViewBoard:
		fallthrough
	default:
		return m.renderBoardView()
	}
}

// rebuildColumns distributes allTasks into the 4 Kanban columns.
func (m *Model) rebuildColumns() {
	newCols := make(map[ColumnType][]meshContext.Task)
	for _, c := range AllColumns {
		newCols[c] = []meshContext.Task{}
	}

	for _, t := range m.allTasks {
		if meshContext.IsHiddenByDefault(t) {
			// Legacy tasks and archived imports stay off the board;
			// 'staypoint task list --legacy' shows them.
			continue
		}
		col := MapTaskToColumn(t)
		newCols[col] = append(newCols[col], t)
	}

	m.columns = newCols

	// Re-bound cursors
	for i, col := range AllColumns {
		count := len(m.columns[col])
		if count == 0 {
			m.columnCursor[i] = 0
		} else if m.columnCursor[i] >= count {
			m.columnCursor[i] = count - 1
		}
	}
}

// Commands (Event-driven without busy polling)

func (m *Model) loadTasksCmd() tea.Cmd {
	return func() tea.Msg {
		if m.db == nil {
			return tasksLoadedMsg{err: fmt.Errorf("no database connection")}
		}
		tasks, err := meshContext.ListTasks(m.db, true)
		return tasksLoadedMsg{tasks: tasks, err: err}
	}
}

func (m *Model) loadThreadCmd(taskID string) tea.Cmd {
	return func() tea.Msg {
		if m.db == nil {
			return threadLoadedMsg{taskID: taskID, err: fmt.Errorf("no database connection")}
		}
		comments, err := meshContext.GetTaskComments(m.db, taskID)
		if err != nil {
			return threadLoadedMsg{taskID: taskID, err: err}
		}
		products, _ := meshContext.GetTaskWorkProducts(m.db, taskID)
		activity, _ := meshContext.GetTaskActivityLog(m.db, taskID)

		return threadLoadedMsg{
			taskID:       taskID,
			comments:     comments,
			workProducts: products,
			activity:     activity,
		}
	}
}

func (m *Model) waitForSSEEventCmd() tea.Cmd {
	return func() tea.Msg {
		evt, ok := <-m.eventsChan
		if !ok {
			return nil
		}
		return daemonEventMsg{event: evt}
	}
}

func (m *Model) waitForSSEStatusCmd() tea.Cmd {
	return func() tea.Msg {
		st, ok := <-m.statusChan
		if !ok {
			return nil
		}
		return daemonStatusMsg{connected: st.connected, err: st.err}
	}
}

func (m *Model) changeStageCmd(taskID, stage string) tea.Cmd {
	return func() tea.Msg {
		err := meshContext.SetTaskExecutionStage(m.db, taskID, stage)
		return stageChangedMsg{taskID: taskID, stage: stage, err: err}
	}
}

func (m *Model) addCommentCmd(taskID, message string) tea.Cmd {
	return func() tea.Msg {
		err := meshContext.AddTaskComment(m.db, taskID, tuiActor(), message)
		return commentAddedMsg{taskID: taskID, err: err}
	}
}

// tuiActor names the local user for comments and activity entries.
func tuiActor() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "user"
}

// toggleBlockCmd flips the block flag and records who did it in activity_log,
// so the task page can say who blocked it and when.
func (m *Model) toggleBlockCmd(task *meshContext.Task) tea.Cmd {
	taskID, blocking := task.ID, !task.IsBlocked
	return func() tea.Msg {
		const reason = "Blocked via TUI"
		var err error
		if blocking {
			err = meshContext.BlockTask(m.db, taskID, reason)
		} else {
			err = meshContext.UnblockTask(m.db, taskID)
		}
		if err == nil {
			logReason := ""
			if blocking {
				logReason = reason
			}
			err = meshContext.LogBlockEvent(m.db, taskID, blocking, tuiActor(), "tui", logReason)
		}
		return blockToggledMsg{taskID: taskID, blocked: blocking, err: err}
	}
}

// Modal View Renderers

func (m *Model) renderMoveModalView() string {
	var b strings.Builder
	title := headerStyle.Render(" Move Task to Stage ")
	b.WriteString(title + "\n\n")

	if m.activeTask != nil {
		b.WriteString(fmt.Sprintf("Task: %s (#%s)\n\n", m.activeTask.Name, m.activeTask.ID[:8]))
	}

	options := []string{
		"[0] / [b]  BACKLOG",
		"[1] / [t]  TODO",
		"[2] / [p]  IN PROGRESS",
		"[3] / [r]  IN REVIEW",
		"[4] / [d]  DONE",
		"[Esc]      Cancel",
	}

	content := strings.Join(options, "\n")
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colBorderActive).
		Padding(1, 2).
		Render(content)

	b.WriteString(box)
	return b.String()
}

func (m *Model) renderCommentInputView() string {
	var b strings.Builder
	title := headerStyle.Render(" Add Comment ")
	b.WriteString(title + "\n\n")

	if m.activeTask != nil {
		b.WriteString(fmt.Sprintf("Task: %s (#%s)\n\n", m.activeTask.Name, m.activeTask.ID[:8]))
	}

	b.WriteString(m.textarea.View())
	b.WriteString("\n\n")
	b.WriteString(helpBarStyle.Render("[Enter / Ctrl+S: Submit]  [Esc: Cancel]"))
	return b.String()
}

func (m *Model) renderHelpView() string {
	var b strings.Builder
	title := headerStyle.Render(" StayPoint Board Shortcuts ")
	b.WriteString(title + "\n\n")

	shortcuts := `
NAVIGATION (Board View):
  h, Left        Move to column on left
  l, Right       Move to column on right
  k, Up          Move to task above
  j, Down        Move to task below
  Enter, Space   Open Thread View for selected task
  m              Move task to another column (todo / in_progress / in_review / done)
  c              Add comment to selected task
  r              Refresh board from SQLite database
  ?              Toggle this help screen
  q, Ctrl+C      Quit application

THREAD VIEW:
  Esc, q         Return to Kanban Board view
  c              Add new comment to thread
  m              Change task stage
  b              Toggle block / unblock task (asks y/n first)
  j, Down        Scroll thread content down
  k, Up          Scroll thread content up

LIVE UPDATES:
  The Board connects to staypointd over Server-Sent Events (SSE).
  When tasks, comments, blockers, or stages change, the board updates
  reactively in real-time matching the database state.
`

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colCyan).
		Padding(1, 2).
		Width(m.width - 6).
		Render(shortcuts)

	b.WriteString(box)
	b.WriteString("\n\n" + helpBarStyle.Render("Press [Esc] or [Enter] to return"))
	return b.String()
}
