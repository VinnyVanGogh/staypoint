package chat

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/adapter"
	"github.com/VinnyVanGogh/staypoint/internal/checkpoint"
	"github.com/VinnyVanGogh/staypoint/internal/config"
	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/conversation"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/google/uuid"
)

// FrameRate30FPS is the tick duration for 30 frames per second (~33ms).
const FrameRate30FPS = 33 * time.Millisecond

type frameTickMsg struct{}

type streamDoneMsg struct {
	usage *adapter.Usage
	err   error
}

type slashResultMsg struct {
	content string
	isError bool
}

// Model represents the Bubble Tea chat TUI state.
type Model struct {
	cfg            Config
	viewport       viewport.Model
	textarea       textarea.Model
	spinner        spinner.Model
	accumulator    *TokenAccumulator
	renderer       *MarkdownRenderer
	store          conversation.Store
	session        *conversation.Conversation
	daemonClient   DaemonClientInterface
	adapterResolve *adapter.Resolver
	customAdapter  adapter.ProviderAdapter

	messages       []ChatMessage
	activeResponse strings.Builder
	streaming      bool
	streamStart    time.Time
	execCancel     context.CancelFunc

	activeModel          string
	activeProvider       string
	activeTask           string
	inProcessMode        bool
	statusMessage        string
	lastExecutedModel    string
	lastExecutedProvider string
	lastExecutedFamily   string
	lastContextWindow    int

	width    int
	height   int
	ready    bool
	quitting bool
}

// NewModel initializes a new Model from Config.
func NewModel(cfg Config) (*Model, error) {
	if cfg.RepoPath == "" {
		if cwd, err := os.Getwd(); err == nil {
			cfg.RepoPath = cwd
		}
	}

	modelName := cfg.Model
	if modelName == "" {
		modelName = DefaultModel
	}
	providerName := cfg.Provider
	if providerName == "" {
		providerName = ResolveProvider(modelName)
	}

	// 1. Textarea setup
	ta := textarea.New()
	ta.Placeholder = "Type a message or slash command (/model, /task, /checkpoint, /undo, /clear, /help)..."
	ta.Focus()
	ta.CharLimit = 100000
	ta.SetHeight(2)
	ta.ShowLineNumbers = false
	ta.Prompt = "❯ "

	// 2. Spinner setup
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("#73daca"))

	// 3. Renderer & accumulator
	renderer := NewMarkdownRenderer(80)
	accumulator := NewTokenAccumulator()

	// 4. Daemon connection or in-process fallback
	var daemonClient DaemonClientInterface
	inProcess := cfg.InProcess
	if !inProcess {
		if cfg.CustomDaemonClient != nil {
			daemonClient = cfg.CustomDaemonClient
		} else {
			client, err := ConnectDaemon(cfg.SocketPath)
			if err == nil {
				daemonClient = client
			} else {
				inProcess = true
			}
		}
	}

	// 5. Database & Conversation Store
	store := cfg.Store
	var dbConn *sql.DB = cfg.DB
	if store == nil {
		if dbConn == nil {
			dbPath := cfg.DBPath
			if dbPath == "" {
				appCfg, _ := config.LoadConfig()
				if appCfg != nil {
					dbPath = appCfg.DBPath
				} else {
					home, _ := os.UserHomeDir()
					dbPath = home + "/.staypoint/staypoint.db"
				}
			}
			s, err := db.Open(dbPath)
			if err == nil {
				dbConn = s.DB()
				store = conversation.NewStore(dbConn)
			}
		} else {
			store = conversation.NewStore(dbConn)
		}
	}

	// 6. Initialize or load Conversation Session
	var sess *conversation.Conversation
	ctx := context.Background()
	if store != nil {
		if cfg.SessionID != "" {
			existing, err := store.GetSession(ctx, cfg.SessionID)
			if err == nil && existing != nil {
				sess = existing
				if cfg.Model == "" && sess.Metadata != nil {
					if m, ok := sess.Metadata["model"].(string); ok && m != "" {
						modelName = m
						providerName = ResolveProvider(modelName)
					}
				}
			}
		}
		if sess == nil {
			sessID := cfg.SessionID
			if sessID == "" {
				sessID = uuid.NewString()
			}
			sess = &conversation.Conversation{
				ID:              sessID,
				Title:           "StayPoint Chat",
				RepoPath:        cfg.RepoPath,
				CreatedAt:       time.Now().UTC(),
				UpdatedAt:       time.Now().UTC(),
				ProviderHandles: make(map[string]*conversation.ProviderHandle),
				Metadata: map[string]interface{}{
					"model":    modelName,
					"provider": providerName,
					"task_id":  cfg.TaskID,
				},
			}
			_ = store.CreateSession(ctx, sess)
		} else if sess.ProviderHandles == nil {
			sess.ProviderHandles = make(map[string]*conversation.ProviderHandle)
		}
	}

	// Load existing messages into TUI
	var msgs []ChatMessage
	lastExecModel := modelName
	lastExecProv := providerName
	lastExecFamily := ProviderFamily(providerName)
	lastCtxWindow := ModelContextWindow(modelName)

	if sess != nil && len(sess.Messages) > 0 {
		for _, m := range sess.Messages {
			role := Role(m.Role)
			msgs = append(msgs, ChatMessage{
				ID:         m.ID,
				Role:       role,
				Content:    m.Content,
				Timestamp:  m.CreatedAt,
				TokenCount: m.TokenCount,
			})
		}

		// Find the last assistant turn to restore last executed model and family
		for i := len(sess.Messages) - 1; i >= 0; i-- {
			if sess.Messages[i].Role == conversation.RoleAssistant {
				if sess.Messages[i].Metadata != nil {
					if mdl, ok := sess.Messages[i].Metadata["model"].(string); ok && mdl != "" {
						lastExecModel = mdl
						lastExecProv = ResolveProvider(mdl)
						lastExecFamily = ProviderFamily(lastExecProv)
						lastCtxWindow = ModelContextWindow(mdl)
						if cfg.Model == "" {
							modelName = mdl
							providerName = lastExecProv
						}
						break
					}
				}
			}
		}
	}

	initialStatus := ""
	if len(msgs) > 0 {
		initialStatus = fmt.Sprintf("Resumed session (%d messages, active: %s)", len(msgs), modelName)
	}

	resolver := cfg.CustomResolver
	if resolver == nil {
		resolver = adapter.NewDefaultResolver()
	}

	m := &Model{
		cfg:                  cfg,
		textarea:             ta,
		spinner:              s,
		accumulator:          accumulator,
		renderer:             renderer,
		store:                store,
		session:              sess,
		daemonClient:         daemonClient,
		adapterResolve:       resolver,
		customAdapter:        cfg.CustomAdapter,
		messages:             msgs,
		activeModel:          modelName,
		activeProvider:       providerName,
		activeTask:           cfg.TaskID,
		inProcessMode:        inProcess || daemonClient == nil || !daemonClient.IsConnected(),
		statusMessage:        initialStatus,
		lastExecutedModel:    lastExecModel,
		lastExecutedProvider: lastExecProv,
		lastExecutedFamily:   lastExecFamily,
		lastContextWindow:    lastCtxWindow,
		width:                80,
		height:               24,
	}

	return m, nil
}

// Init initializes the Bubble Tea program.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		m.spinner.Tick,
	)
}

// Update handles incoming messages and events.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			if m.streaming {
				// Cancel generation and kill the subprocess group immediately
				if m.execCancel != nil {
					m.execCancel()
					m.execCancel = nil
				}
				m.streaming = false
				m.statusMessage = "[Generation cancelled by user]"
				m.finalizeAssistantMessage(nil, errors.New("cancelled by user"))
				return m, nil
			}
			m.quitting = true
			if m.daemonClient != nil {
				_ = m.daemonClient.Close()
			}
			return m, tea.Quit

		case tea.KeyEnter:
			// Alt+Enter / Shift+Enter for newline, Enter submits
			if msg.Alt {
				m.textarea.InsertString("\n")
				return m, nil
			}
			input := strings.TrimSpace(m.textarea.Value())
			if input == "" {
				return m, nil
			}
			m.textarea.Reset()

			if strings.HasPrefix(input, "/") {
				cmd := m.handleSlashCommand(input)
				return m, cmd
			}

			// Normal user chat turn
			cmd := m.submitUserMessage(input)
			return m, cmd

		case tea.KeyPgUp, tea.KeyPgDown:
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}

	case tea.WindowSizeMsg:
		m.handleWindowSize(msg.Width, msg.Height)
		return m, nil

	case frameTickMsg:
		if m.streaming {
			tokens := m.accumulator.Flush()
			if len(tokens) > 0 {
				m.activeResponse.WriteString(tokens)
				m.refreshViewport(true)
			}
			// Schedule next frame tick (~30fps)
			return m, tea.Tick(FrameRate30FPS, func(t time.Time) tea.Msg {
				return frameTickMsg{}
			})
		}
		return m, nil

	case streamDoneMsg:
		m.streaming = false
		m.execCancel = nil
		// Final flush of remaining accumulated tokens
		tokens := m.accumulator.Flush()
		if len(tokens) > 0 {
			m.activeResponse.WriteString(tokens)
		}
		m.finalizeAssistantMessage(msg.usage, msg.err)
		return m, nil

	case slashResultMsg:
		role := RoleSystem
		if msg.isError {
			role = RoleError
		}
		m.messages = append(m.messages, ChatMessage{
			ID:        uuid.NewString(),
			Role:      role,
			Content:   msg.content,
			Timestamp: time.Now(),
			IsError:   msg.isError,
		})
		m.refreshViewport(true)
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if m.streaming {
			m.refreshViewport(false)
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)
	}

	var taCmd, vpCmd tea.Cmd
	m.textarea, taCmd = m.textarea.Update(msg)
	m.viewport, vpCmd = m.viewport.Update(msg)
	cmds = append(cmds, taCmd, vpCmd)
	return m, tea.Batch(cmds...)
}

func (m *Model) handleWindowSize(w, h int) {
	m.width = w
	m.height = h

	headerHeight := 3
	textareaHeight := 4
	helpHeight := 1
	vpHeight := h - headerHeight - textareaHeight - helpHeight
	if vpHeight < 4 {
		vpHeight = 4
	}

	contentWidth := w - 4
	if contentWidth < 20 {
		contentWidth = 20
	}
	m.renderer.SetWidth(contentWidth)

	if !m.ready {
		m.viewport = viewport.New(w, vpHeight)
		m.viewport.SetContent("")
		m.ready = true
	} else {
		m.viewport.Width = w
		m.viewport.Height = vpHeight
	}

	m.textarea.SetWidth(w)
	m.refreshViewport(true)
}

func (m *Model) submitUserMessage(content string) tea.Cmd {
	userMsg := ChatMessage{
		ID:        uuid.NewString(),
		Role:      RoleUser,
		Content:   content,
		Timestamp: time.Now(),
	}
	m.messages = append(m.messages, userMsg)

	// Persist to conversation.Store
	if m.store != nil && m.session != nil {
		ctx := context.Background()
		_ = m.store.AppendMessage(ctx, m.session.ID, &conversation.Message{
			ID:        userMsg.ID,
			SessionID: m.session.ID,
			Role:      conversation.RoleUser,
			Content:   userMsg.Content,
			CreatedAt: userMsg.Timestamp.UTC(),
		})
	}

	m.activeResponse.Reset()
	m.accumulator.Reset()
	m.streaming = true
	m.streamStart = time.Now()
	m.refreshViewport(true)

	// Start streaming goroutine & 30fps ticker
	execCmd := m.startAdapterExecution(content)
	tickCmd := tea.Tick(FrameRate30FPS, func(t time.Time) tea.Msg {
		return frameTickMsg{}
	})

	return tea.Batch(execCmd, tickCmd, m.spinner.Tick)
}

func (m *Model) startAdapterExecution(prompt string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(context.Background())
		m.execCancel = cancel

		var provAdapter adapter.ProviderAdapter
		if m.customAdapter != nil {
			provAdapter = m.customAdapter
		} else {
			provAdapter = adapter.AdapterFor(m.activeProvider)
		}

		bin, err := m.adapterResolve.Resolve(provAdapter)
		if err != nil && provAdapter.BinaryName() != "" {
			return streamDoneMsg{
				err: fmt.Errorf("provider CLI %q not found: %w", provAdapter.BinaryName(), err),
			}
		}

		currentFamily := ProviderFamily(m.activeProvider)
		sameFamily := (m.lastExecutedFamily == currentFamily) && len(m.messages) > 1

		var nativeHandle string
		if m.session != nil && m.session.ProviderHandles != nil {
			if h, ok := m.session.ProviderHandles[m.activeProvider]; ok && h != nil {
				nativeHandle = h.Handle
			}
		}

		useNativeResume := sameFamily && nativeHandle != ""
		downshift := IsDownshift(m.lastExecutedModel, m.activeModel)

		execPrompt := prompt
		var history []adapter.ChatMessageInput

		if useNativeResume {
			// Native resume within same family: pass user prompt and native session flag!
			m.statusMessage = fmt.Sprintf("✓ Native %s resume active (%s)", currentFamily, nativeHandle)
		} else {
			// Cross-family switch, first turn, or quota failover: rehydrate from chat_messages!
			// Exclude the current user message which was just appended (last message in m.messages)
			priorMsgs := m.messages
			if len(priorMsgs) > 0 && priorMsgs[len(priorMsgs)-1].Role == RoleUser {
				priorMsgs = priorMsgs[:len(priorMsgs)-1]
			}
			if len(priorMsgs) > 0 {
				targetWindow := ModelContextWindow(m.activeModel)
				execPrompt, history = BuildRehydratedTurn(priorMsgs, prompt, downshift, targetWindow)
				if downshift {
					m.statusMessage = fmt.Sprintf("⚠ Downshift to %s: rehydrated & condensed history", m.activeModel)
				} else {
					m.statusMessage = fmt.Sprintf("⚠ Cross-family switch to %s: rehydrated history", currentFamily)
				}
			}
		}

		pr, pw := io.Pipe()

		execReq := adapter.ExecRequest{
			Bin: bin,
			Dir: m.cfg.RepoPath,
			Opts: adapter.ParsedOptions{
				Prompt:         execPrompt,
				Model:          m.activeModel,
				ConversationID: nativeHandle,
				OutputFormat:   "stream-json",
				History:        history,
			},
			Stdout: pw,
			Stderr: io.Discard,
		}

		errChan := make(chan error, 1)
		go func() {
			defer pw.Close()
			errChan <- provAdapter.Execute(ctx, execReq)
		}()

		var usage *adapter.Usage
		scanner := bufio.NewScanner(pr)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}

			deltas, err := provAdapter.ParseStreamDelta(line)
			if err != nil {
				// Fallback: raw line output
				m.accumulator.Append(string(line) + "\n")
				continue
			}

			for _, d := range deltas {
				if d.SessionID != "" && m.session != nil {
					// Capture native session ID from the adapter
					if m.session.ProviderHandles == nil {
						m.session.ProviderHandles = make(map[string]*conversation.ProviderHandle)
					}
					m.session.ProviderHandles[m.activeProvider] = &conversation.ProviderHandle{
						SessionID: m.session.ID,
						Provider:  m.activeProvider,
						Handle:    d.SessionID,
						Model:     m.activeModel,
						CreatedAt: time.Now().UTC(),
						UpdatedAt: time.Now().UTC(),
					}
					if m.store != nil {
						_ = m.store.SetProviderHandle(context.Background(), m.session.ProviderHandles[m.activeProvider])
					}
				}

				switch d.Kind {
				case adapter.DeltaText:
					m.accumulator.Append(d.Text)
				case adapter.DeltaThinking:
					m.accumulator.Append(d.Text)
				case adapter.DeltaResult:
					if d.Text != "" && m.accumulator.TotalLen() == 0 {
						m.accumulator.Append(d.Text)
					}
					if d.Usage != nil {
						usage = d.Usage
					}
				case adapter.DeltaUsage:
					usage = d.Usage
				case adapter.DeltaError:
					if d.Error != "" {
						return streamDoneMsg{err: errors.New(d.Error), usage: usage}
					}
				}
			}
		}

		execErr := <-errChan
		return streamDoneMsg{err: execErr, usage: usage}
	}
}

func (m *Model) finalizeAssistantMessage(usage *adapter.Usage, err error) {
	content := m.activeResponse.String()
	if err != nil && !errors.Is(err, context.Canceled) {
		if content == "" {
			content = fmt.Sprintf("Error: %v", err)
		} else {
			content += fmt.Sprintf("\n\n*Error: %v*", err)
		}
	}

	tokenCount := 0
	if usage != nil {
		tokenCount = int(usage.OutputTokens)
	}

	asstMsg := ChatMessage{
		ID:         uuid.NewString(),
		Role:       RoleAssistant,
		Model:      m.activeModel,
		Content:    content,
		Timestamp:  time.Now(),
		TokenCount: tokenCount,
		IsError:    err != nil,
	}
	m.messages = append(m.messages, asstMsg)
	m.activeResponse.Reset()

	// Persist to conversation.Store
	if m.store != nil && m.session != nil {
		ctx := context.Background()
		_ = m.store.AppendMessage(ctx, m.session.ID, &conversation.Message{
			ID:         asstMsg.ID,
			SessionID:  m.session.ID,
			Role:       conversation.RoleAssistant,
			Content:    asstMsg.Content,
			TokenCount: asstMsg.TokenCount,
			CreatedAt:  asstMsg.Timestamp.UTC(),
			Metadata: map[string]interface{}{
				"model":    m.activeModel,
				"provider": m.activeProvider,
			},
		})

		// Update session metadata in store
		if m.session.Metadata == nil {
			m.session.Metadata = make(map[string]interface{})
		}
		m.session.Metadata["model"] = m.activeModel
		m.session.Metadata["provider"] = m.activeProvider
		m.session.Metadata["family"] = ProviderFamily(m.activeProvider)
		_ = m.store.UpdateSession(context.Background(), m.session)
	}

	// Update last executed state
	m.lastExecutedModel = m.activeModel
	m.lastExecutedProvider = m.activeProvider
	m.lastExecutedFamily = ProviderFamily(m.activeProvider)
	m.lastContextWindow = ModelContextWindow(m.activeModel)

	m.refreshViewport(true)
}

func (m *Model) handleSlashCommand(line string) tea.Cmd {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return nil
	}
	cmd := parts[0]
	args := parts[1:]

	return func() tea.Msg {
		switch cmd {
		case "/model":
			if len(args) == 0 {
				return slashResultMsg{
					content: fmt.Sprintf("Active model: **%s** (provider: `%s`, family: `%s`)\n\nAvailable models:\n- `gemini-2.5-flash` / `gemini-2.5-pro` / `gemini-3.1-pro-high`\n- `claude-3-7-sonnet` / `claude-3-5-sonnet` / `claude-3-5-haiku`\n- `codex` / `o1` / `o3`\n- `ollama` / `local`\n\nUsage: `/model <name>`", m.activeModel, m.activeProvider, ProviderFamily(m.activeProvider)),
				}
			}
			newModel := args[0]
			newProv := ResolveProvider(newModel)
			oldFamily := ProviderFamily(m.activeProvider)
			newFamily := ProviderFamily(newProv)
			oldModel := m.activeModel

			m.activeModel = newModel
			m.activeProvider = newProv

			// Record handle in conversation store if active
			if m.store != nil && m.session != nil {
				_ = m.store.SetProviderHandle(context.Background(), &conversation.ProviderHandle{
					SessionID: m.session.ID,
					Provider:  newProv,
					Handle:    newModel,
					Model:     newModel,
					CreatedAt: time.Now().UTC(),
					UpdatedAt: time.Now().UTC(),
				})
			}

			downshift := IsDownshift(oldModel, newModel)
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("✓ Switched active model to **%s** (provider: `%s`, family: `%s`)", newModel, newProv, newFamily))

			if oldFamily != newFamily && len(m.messages) > 0 {
				m.statusMessage = fmt.Sprintf("⚠ Warning: Model family switch (%s ➔ %s) reprocesses input tokens from history.", oldFamily, newFamily)
				sb.WriteString(fmt.Sprintf("\n\n> ⚠ **Family switch (%s ➔ %s)**: Prompt cache invalidated. Input tokens will be reprocessed from conversation history.", oldFamily, newFamily))
				if downshift {
					sb.WriteString(fmt.Sprintf("\n> ℹ **Downshifting context window** (%dk ➔ %dk): History will be condensed using `internal/condenser`.", ModelContextWindow(oldModel)/1000, ModelContextWindow(newModel)/1000))
				}
			} else if oldFamily == newFamily && len(m.messages) > 0 {
				m.statusMessage = fmt.Sprintf("✓ Retained %s provider family (native session resume active).", newFamily)
				sb.WriteString(fmt.Sprintf("\n\n> ✓ **Same family (%s)**: Native session resume active. Prompt cache preserved.", newFamily))
			}

			return slashResultMsg{
				content: sb.String(),
			}

		case "/task":
			if len(args) == 0 {
				if m.activeTask != "" {
					return slashResultMsg{content: fmt.Sprintf("Active task: `%s`", m.activeTask)}
				}
				// Query recent tasks
				if m.cfg.DB != nil {
					tasks, err := meshContext.ListTasks(m.cfg.DB, false)
					tasks = meshContext.FilterLegacy(tasks, false)
					if err == nil && len(tasks) > 0 {
						var sb strings.Builder
						sb.WriteString("Active Tasks:\n")
						for _, t := range tasks {
							sb.WriteString(fmt.Sprintf("- `%s`: %s [%s]\n", t.ID, t.Name, t.Status))
						}
						sb.WriteString("\nUsage: `/task <id>` to bind to a task")
						return slashResultMsg{content: sb.String()}
					}
				}
				return slashResultMsg{content: "No active task bound. Usage: `/task <id>`"}
			}
			m.activeTask = args[0]
			return slashResultMsg{content: fmt.Sprintf("✓ Bound chat session to task: `%s`", m.activeTask)}

		case "/checkpoint":
			msg := strings.Join(args, " ")
			if msg == "" {
				msg = fmt.Sprintf("chat checkpoint %s", time.Now().Format("15:04:05"))
			}

			// Try IPC first if connected
			if m.daemonClient != nil && m.daemonClient.IsConnected() {
				res, err := m.daemonClient.Checkpoint(context.Background(), msg)
				if err == nil {
					return slashResultMsg{content: fmt.Sprintf("✓ Checkpoint created via staypointd:\n%s", res)}
				}
			}

			// Fallback: in-process checkpoint
			cp, err := checkpoint.CreateCheckpoint(context.Background(), checkpoint.CreateOptions{
				WorkDir: m.cfg.RepoPath,
				Message: msg,
			})
			if err != nil {
				return slashResultMsg{content: fmt.Sprintf("Checkpoint failed: %v", err), isError: true}
			}
			return slashResultMsg{
				content: fmt.Sprintf("✓ Checkpoint created (in-process):\n- ID: `%s`\n- Commit: `%s`\n- Ref: `%s`", cp.ID, cp.CommitSHA, cp.Ref),
			}

		case "/undo":
			targetID := ""
			if len(args) > 0 {
				targetID = args[0]
			}

			// Try IPC first if connected
			if m.daemonClient != nil && m.daemonClient.IsConnected() {
				res, err := m.daemonClient.Undo(context.Background(), targetID)
				if err == nil {
					return slashResultMsg{content: fmt.Sprintf("✓ Undo completed via staypointd:\n%s", res)}
				}
			}

			// Fallback: in-process undo
			undoRes, err := checkpoint.Undo(context.Background(), checkpoint.UndoOptions{
				WorkDir:      m.cfg.RepoPath,
				CheckpointID: targetID,
			})
			if err != nil {
				return slashResultMsg{content: fmt.Sprintf("Undo failed: %v", err), isError: true}
			}
			safetyBackup := "none"
			if undoRes.SafetyCP != nil {
				safetyBackup = undoRes.SafetyCP.CommitSHA
			}
			return slashResultMsg{
				content: fmt.Sprintf("✓ Restored working tree (in-process):\n- Target: `%s`\n- Files Reverted: %d\n- Safety Backup: `%s`", undoRes.RestoredTo.CommitSHA, len(undoRes.FilesReverted), safetyBackup),
			}

		case "/clear":
			m.messages = nil
			m.refreshViewport(true)
			return slashResultMsg{content: "Chat history cleared."}

		case "/help":
			help := `### StayPoint Chat Slash Commands
- **` + "`/model [name]`" + `**: View or switch the active LLM provider/model
- **` + "`/task [id]`" + `**: View or bind the active task ID
- **` + "`/checkpoint [msg]`" + `**: Take an ephemeral Git micro-checkpoint (<15ms)
- **` + "`/undo [id]`" + `**: Restore working tree to latest or specific checkpoint
- **` + "`/clear`" + `**: Clear the active chat screen
- **` + "`/help`" + `**: Show this help message

### Keybindings
- **Enter**: Send message / execute slash command
- **Alt+Enter**: Insert newline in prompt
- **Ctrl+C**: Cancel active response (kills process group) / Exit chat
- **PgUp / PgDown**: Scroll conversation history`
			return slashResultMsg{content: help}

		default:
			return slashResultMsg{
				content: fmt.Sprintf("Unknown command: `%s`. Type `/help` for available commands.", cmd),
				isError: true,
			}
		}
	}
}

func (m *Model) refreshViewport(scrollToBottom bool) {
	var sb strings.Builder

	for _, msg := range m.messages {
		sb.WriteString(m.renderer.RenderMessage(msg))
		sb.WriteString("\n")
	}

	if m.streaming {
		content := m.activeResponse.String()
		sb.WriteString(m.renderer.RenderStreamingAssistant(m.activeModel, content, m.spinner.View(), m.streamStart))
	}

	m.viewport.SetContent(sb.String())
	if scrollToBottom {
		m.viewport.GotoBottom()
	}
}

// View renders the full terminal UI layout.
func (m *Model) View() string {
	if !m.ready {
		return "Initializing StayPoint Chat..."
	}

	// 1. Header Bar
	daemonBadge := lipgloss.NewStyle().
		Background(lipgloss.Color("#24283b")).
		Foreground(lipgloss.Color("#e0af68")).
		Render("[in-process mode]")

	if m.daemonClient != nil && m.daemonClient.IsConnected() {
		daemonBadge = lipgloss.NewStyle().
			Background(lipgloss.Color("#1f2335")).
			Foreground(lipgloss.Color("#9ece6a")).
			Render("[staypointd: connected]")
	}

	sessionID := "new"
	if m.session != nil {
		if len(m.session.ID) > 12 {
			sessionID = m.session.ID[:12]
		} else {
			sessionID = m.session.ID
		}
	}

	taskBadge := ""
	if m.activeTask != "" {
		taskBadge = fmt.Sprintf(" │ Task: %s", m.activeTask)
	}

	header := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#7aa2f7")).
		Render(fmt.Sprintf(" StayPoint Chat │ Session: %s │ Model: %s%s  %s", sessionID, m.activeModel, taskBadge, daemonBadge))

	divider := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#3b4261")).
		Render(strings.Repeat("─", m.width))

	// 2. Viewport
	vpView := m.viewport.View()

	// 3. Textarea Input Box
	inputBox := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), true, false, false, false).
		BorderForeground(lipgloss.Color("#3b4261")).
		Render(m.textarea.View())

	// 4. Status & Shortcuts Bar
	statusText := "Enter: Send │ Alt+Enter: Newline │ Ctrl+C: Cancel/Exit │ /help: Commands"
	if m.statusMessage != "" {
		statusText = m.statusMessage + "  " + statusText
	}

	footer := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#565f89")).
		Faint(true).
		Render(statusText)

	return fmt.Sprintf("%s\n%s\n%s\n%s\n%s", header, divider, vpView, inputBox, footer)
}
