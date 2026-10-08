package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/ai"
	"github.com/VinnyVanGogh/staypoint/internal/bridge"
	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/paperclip"
	"github.com/VinnyVanGogh/staypoint/internal/ui"
	"github.com/charmbracelet/glamour"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"testing"
)

var taskCreateCmd = &cobra.Command{
	Use:     "create [comment]",
	Aliases: []string{"generate", "new"},
	Short:   "Generate a structured engineering task using Gemini & Claude with TUI or CLI input",
	Long: `Dual-mode dynamic task generator.
In TUI mode (no arguments or --tui), launches an interactive modal textarea.
In CLI mode, accepts raw text arguments or piped stdin.
Infers structured fields via Gemini 3.8 Flash (falling back to Claude Sonnet),
dispatches to the Paperclip API, renders a markdown summary via glamour,
and outputs token telemetry alongside pacing status.`,
	RunE: runTaskCreate,
}

func init() {
	taskCmd.AddCommand(taskCreateCmd)
	taskCreateCmd.Flags().Bool("tui", false, "Force launch Bubble Tea TUI interactive textarea")
	taskCreateCmd.Flags().Bool("dry-run", false, "Infer task and render summary without dispatching to Paperclip API")
	taskCreateCmd.Flags().BoolP("yes", "y", false, "Skip interactive confirmation/action card and dispatch immediately")
	taskCreateCmd.Flags().BoolP("interactive", "i", false, "Force interactive disposition and clarification prompters")
	taskCreateCmd.Flags().BoolP("backlog", "b", false, "Park task unassigned in backlog (zero prompts)")
	taskCreateCmd.Flags().BoolP("start", "s", false, "Assign task immediately to Chief of Staff and schedule active execution")
	taskCreateCmd.Flags().Bool("assign", false, "Alias for --start")
	taskCreateCmd.Flags().String("company", "", "Target Paperclip company ID (defaults to PAPERCLIP_COMPANY_ID)")
	taskCreateCmd.Flags().String("project", "", "Target project ID (defaults to current project)")
	taskCreateCmd.Flags().String("priority", "", "Override priority (low, medium, high, urgent)")
	taskCreateCmd.Flags().String("role", "", "Override assignee role or agent")
	taskCreateCmd.Flags().Float64("budget", 0.0, "Maximum budget limit in USD")
	taskCreateCmd.Flags().Int("max-turns", 0, "Maximum allowed turns")
	taskCreateCmd.Flags().Bool("ai", true, "Force dynamic AI inference")
}

func runTaskCreate(cmd *cobra.Command, args []string) error {
	if childCreateRequested(cmd) {
		return runTaskCreateChild(cmd, args) // STA-820
	}
	if localOrgCreateRequested(cmd) {
		return runTaskCreateLocalOrg(cmd, args) // STA-854
	}
	// STA-838: validate --kind/--provider/--model before any inference or
	// dispatch, so a refused choice (gemini on a code kind) costs nothing.
	kindFlag, _ := cmd.Flags().GetString("kind")
	if kindFlag != "" && !meshContext.IsValidWorkKind(kindFlag) {
		return fmt.Errorf("invalid --kind %q: must be one of %s", kindFlag, strings.Join(meshContext.ValidWorkKinds(), ", "))
	}
	choice, err := taskChoiceFromFlags(cmd, kindFlag, "")
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	tuiFlag, _ := cmd.Flags().GetBool("tui")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	companyFlag, _ := cmd.Flags().GetString("company")
	projectFlag, _ := cmd.Flags().GetString("project")
	priorityOverride, _ := cmd.Flags().GetString("priority")
	roleOverride, _ := cmd.Flags().GetString("role")

	var rawComment string

	// Determine whether to use TUI mode or CLI mode
	isTerminal := isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())

	if tuiFlag || (len(args) == 0 && isTerminal) {
		// TUI Mode
		val, err := ui.RunTextareaModal()
		if err != nil {
			if err == ui.ErrCancelled {
				fmt.Fprintln(out, "\n\033[1;33m[StayPoint Task Generator]\033[0m Task creation cancelled.")
				return nil
			}
			return fmt.Errorf("TUI error: %w", err)
		}
		rawComment = val
	} else if len(args) > 0 {
		// CLI arguments mode
		rawComment = strings.Join(args, " ")
	} else if !isTerminal {
		// Piped stdin mode
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("failed to read from stdin: %w", err)
		}
		rawComment = string(data)
	}

	rawComment = sanitizeComment(rawComment)
	if strings.TrimSpace(rawComment) == "" {
		fmt.Fprintln(out, "\033[1;33m[StayPoint Task Generator]\033[0m Empty comment provided. No task created.")
		return nil
	}

	fmt.Fprintln(out, "\033[1;36m[StayPoint :: Task Generation Engine]\033[0m Ingesting input & structuring issue...")

	ctx := context.Background()

	// 0. Fetch Project Candidates
	paperclipClient := paperclip.NewClient("", "")
	targetCompany := companyFlag
	if targetCompany == "" {
		targetCompany = os.Getenv("PAPERCLIP_COMPANY_ID")
	}
	var projectCandidates []string
	if targetCompany != "" {
		fetchCtx, fetchCancel := context.WithTimeout(ctx, 15*time.Second)
		projects, err := paperclipClient.FetchProjects(fetchCtx, targetCompany)
		fetchCancel()
		if err == nil {
			for _, p := range projects {
				projectCandidates = append(projectCandidates, p.Name)
			}
		}
	}

	// 1. Run AI Inference
	genCfg := ai.DefaultGeneratorConfig()
	generator := ai.NewGenerator(genCfg)

	genCtx, genCancel := context.WithTimeout(ctx, 45*time.Second)
	genResult, err := generator.GenerateTask(genCtx, rawComment, projectCandidates)
	genCancel()
	if err != nil {
		return fmt.Errorf("inference error: %w", err)
	}

	if priorityOverride != "" {
		genResult.Task.Priority = strings.ToLower(priorityOverride)
	}
	if roleOverride != "" {
		genResult.Task.AssigneeRole = roleOverride
	}

	// Output immediate confirmation for CLI and e2e tests
	fmt.Fprintf(out, "\033[1;32m✔ Task created:\033[0m %s\n\n", genResult.Task.Title)

	// Pre-dispatch Interactive Flow: Disposition Prompter, Clarification Modal, and 1-Key Action Card
	interactive := (isTerminal || tuiFlag) && !dryRun && !testing.Testing()
	yesFlag, _ := cmd.Flags().GetBool("yes")
	if yesFlag {
		interactive = false
	}
	interactiveFlag, _ := cmd.Flags().GetBool("interactive")
	if interactiveFlag {
		interactive = true
	}
	backlogFlag, _ := cmd.Flags().GetBool("backlog")
	startFlag, _ := cmd.Flags().GetBool("start")
	assignFlag, _ := cmd.Flags().GetBool("assign")
	if assignFlag {
		startFlag = true
	}

	if backlogFlag {
		genResult.Task.Status = "backlog"
		interactive = false
	} else if startFlag {
		genResult.Task.Status = "todo"
		interactive = false
	}

	// 3. Dispatch to Paperclip API
	companyID := companyFlag
	var companyPrefix string

	if companyID != "" {
		compCtx, compCancel := context.WithTimeout(ctx, 15*time.Second)
		comp, compErr := paperclipClient.GetCompany(compCtx, companyID)
		compCancel()
		if compErr == nil && comp != nil {
			companyPrefix = comp.IssuePrefix
		}
	} else {
		// Dynamically resolve company based on inferred Organization first
		if genResult.Task.Organization != "" {
			compCtx, compCancel := context.WithTimeout(ctx, 15*time.Second)
			comp, compErr := paperclipClient.ResolveCompany(compCtx, genResult.Task.Organization)
			compCancel()
			if compErr == nil && comp != nil {
				companyID = comp.ID
				companyPrefix = comp.IssuePrefix
			}
		}
		// If still empty, fall back to environment variable
		if companyID == "" {
			companyID = os.Getenv("PAPERCLIP_COMPANY_ID")
			compCtx, compCancel := context.WithTimeout(ctx, 15*time.Second)
			comp, compErr := paperclipClient.GetCompany(compCtx, companyID)
			compCancel()
			if compErr == nil && comp != nil {
				companyPrefix = comp.IssuePrefix
			}
		}
	}

	projectID := projectFlag
	if projectID == "" {
		projectID = os.Getenv("PAPERCLIP_PROJECT_ID")
	}
	if projectID == "" && companyID != "" {
		projCtx, projCancel := context.WithTimeout(ctx, 15*time.Second)
		resolvedProjID, projErr := paperclipClient.ResolveProject(projCtx, companyID, genResult.Task.Project)
		projCancel()
		if projErr == nil && resolvedProjID != "" {
			projectID = resolvedProjID
		}
	}

	var assigneeID string
	var assigneeName string
	if companyID != "" && genResult.Task.Status != "backlog" {
		listCtx, listCancel := context.WithTimeout(ctx, 15*time.Second)
		agents, agentErr := paperclipClient.ListAgents(listCtx, companyID)
		listCancel()
		if agentErr == nil {
			if roleOverride != "" {
				lowerRole := strings.ToLower(roleOverride)
				for _, agent := range agents {
					if strings.EqualFold(agent.Role, lowerRole) || strings.Contains(strings.ToLower(agent.Name), lowerRole) {
						assigneeID = agent.ID
						assigneeName = agent.Name
						break
					}
				}
			}
			if assigneeID == "" {
				for _, agent := range agents {
					if strings.Contains(strings.ToLower(agent.Name), "chief of staff") || strings.EqualFold(agent.Role, "ceo") {
						assigneeID = agent.ID
						assigneeName = agent.Name
						break
					}
				}
			}
		}
	} else if genResult.Task.Status == "backlog" {
		assigneeID = ""
		assigneeName = "Unassigned"
	}

	for interactive {
		choice, newStatus, err := ui.RunActionCard(genResult.Task, assigneeName, assigneeID)
		if err != nil || choice == ui.ActionCancel {
			fmt.Fprintln(out, "\n\033[1;33m[StayPoint Task Generator]\033[0m Task creation cancelled.")
			return nil
		}
		if newStatus != "" {
			genResult.Task.Status = newStatus
		}

		switch choice {
		case ui.ActionDispatch:
			interactive = false
		case ui.ActionDispatchBacklog:
			genResult.Task.Status = "backlog"
			assigneeID = ""
			assigneeName = "Unassigned"
			interactive = false
		case ui.ActionDispatchActive:
			genResult.Task.Status = "todo"
			if (assigneeID == "" || assigneeName == "Unassigned") && companyID != "" {
				listCtx, listCancel := context.WithTimeout(ctx, 15*time.Second)
				agents, agentErr := paperclipClient.ListAgents(listCtx, companyID)
				listCancel()
				if agentErr == nil {
					for _, agent := range agents {
						if strings.Contains(strings.ToLower(agent.Name), "chief of staff") || strings.EqualFold(agent.Role, "ceo") {
							assigneeID = agent.ID
							assigneeName = agent.Name
							break
						}
					}
				}
			}
			interactive = false
		case ui.ActionPromptDisposition:
			selectedDisp, err := ui.RunDispositionPrompter(genResult.Task.Status)
			if err == nil && selectedDisp != "" {
				genResult.Task.Status = selectedDisp
				if selectedDisp == "backlog" {
					assigneeID = ""
					assigneeName = "Unassigned"
				} else if (assigneeID == "" || assigneeName == "Unassigned") && companyID != "" {
					listCtx, listCancel := context.WithTimeout(ctx, 15*time.Second)
					agents, agentErr := paperclipClient.ListAgents(listCtx, companyID)
					listCancel()
					if agentErr == nil {
						for _, agent := range agents {
							if strings.Contains(strings.ToLower(agent.Name), "chief of staff") || strings.EqualFold(agent.Role, "ceo") {
								assigneeID = agent.ID
								assigneeName = agent.Name
								break
							}
						}
					}
				}
			}
		case ui.ActionClarify:
			clarificationText, err := ui.RunClarificationModal(genResult.Task.AskClarification)
			if err == nil && strings.TrimSpace(clarificationText) != "" {
				fmt.Fprintln(out, "\033[1;36m[StayPoint :: Task Generation Engine]\033[0m Re-synthesizing task with clarification...")
				updatedComment := fmt.Sprintf("%s\n\nClarification Response:\n%s", rawComment, clarificationText)
				reGenCtx, reGenCancel := context.WithTimeout(ctx, 45*time.Second)
				newGenResult, genErr := generator.GenerateTask(reGenCtx, updatedComment, projectCandidates)
				reGenCancel()
				if genErr == nil {
					genResult = newGenResult
					genResult.Task.AskClarification = ""
				} else {
					genResult.Task.Description += fmt.Sprintf("\n\n### Clarification\n%s", clarificationText)
					genResult.Task.AskClarification = ""
				}
				if priorityOverride != "" {
					genResult.Task.Priority = strings.ToLower(priorityOverride)
				}
				if roleOverride != "" {
					genResult.Task.AssigneeRole = roleOverride
				}
			}
		case ui.ActionEdit:
			if editedDesc, err := ui.RunTextareaModal(); err == nil && strings.TrimSpace(editedDesc) != "" {
				genResult.Task.Description = editedDesc
			}
		}
	}

	if !interactive && genResult.Task.AskClarification != "" {
		if !strings.Contains(genResult.Task.Description, "## Assumptions Made") {
			genResult.Task.Description += fmt.Sprintf("\n\n## Assumptions Made\n- %s (clarification bypassed in non-interactive mode)", genResult.Task.AskClarification)
		}
		if genResult.Task.Status == "" {
			genResult.Task.Status = "backlog"
		}
		genResult.Task.AskClarification = ""
	}

	if genResult.Task.AskClarification != "" {
		fmt.Fprintf(out, "\033[1;33m⚠️  AI Clarification Note:\033[0m %s\n\n", genResult.Task.AskClarification)
	}

	// 2. Render Markdown Summary via Glamour
	renderedCard := renderMarkdownSummary(genResult.Task, assigneeName, assigneeID)
	fmt.Fprint(out, renderedCard)

	var issueResp *paperclip.IssueResponse
	var issueURL string

	if !dryRun && companyID != "" {
		req := paperclip.CreateIssueRequest{
			Title:           genResult.Task.Title,
			Description:     genResult.Task.Description,
			Priority:        genResult.Task.Priority,
			ProjectId:       projectID,
			AssigneeAgentId: assigneeID,
			Labels:          genResult.Task.Labels,
			Status:          genResult.Task.Status,
		}

		if genResult.Task.Status == "backlog" {
			req.AssigneeAgentId = "" // Unassigned backlog dispatch to save tokens
		}

		apiCtx, apiCancel := context.WithTimeout(ctx, 15*time.Second)
		resp, err := paperclipClient.CreateIssue(apiCtx, companyID, req)
		apiCancel()
		if err != nil {
			fmt.Fprintf(out, "\033[1;33m⚠ Paperclip dispatch notice:\033[0m %v (saving locally)\n", err)
		} else {
			issueResp = resp
			if companyPrefix == "" {
				companyPrefix = "STA"
			}
			issueURL = paperclipClient.IssueURL(companyPrefix, issueResp.ID)
		}
	} else if dryRun {
		fmt.Fprintln(out, "\033[1;33m[Dry Run Mode]\033[0m Skipped Paperclip API dispatch.")
	} else if companyID == "" {
		fmt.Fprintln(out, "\033[1;33m⚠ Paperclip dispatch notice:\033[0m Paperclip company could not be resolved or server unreachable (saving locally).")
	}

	// 4. Save to local StayPoint DB if initialized
	if cfg != nil && cfg.DBPath != "" {
		if store, err := db.Open(cfg.DBPath); err == nil {
			defer store.Close()
			cwd, _ := os.Getwd()
			branch := meshContext.GetCurrentGitBranch(cwd)
			role := "personal"
			if bridge.IsWorkRepo(cwd) {
				role = "work"
			}
			budget, _ := cmd.Flags().GetFloat64("budget")
			maxTurns, _ := cmd.Flags().GetInt("max-turns")
			// STA-861: --session means an interactive session tracks this
			// task: park it in backlog (never claimed) and attach.
			sessionFlag, _ := cmd.Flags().GetString("session")
			// --backlog parks the local task too; without it the create-time
			// assignment wake started a run the Board never asked for.
			stage := ""
			if strings.TrimSpace(sessionFlag) != "" || genResult.Task.Status == "backlog" {
				stage = governance.StageBacklog
			}
			created, createErr := meshContext.CreateTaskWithOptions(store.DB(), meshContext.TaskCreateOptions{
				Name:           genResult.Task.Title,
				RepoPath:       cwd,
				GitBranch:      branch,
				AccountRole:    role,
				MaxBudgetUSD:   budget,
				MaxTurns:       maxTurns,
				Organization:   genResult.Task.Organization,
				Project:        genResult.Task.Project,
				WorkKind:       kindFlag,
				Provider:       choice.Provider,
				ModelOverride:  choice.Model,
				ExecutionStage: stage,
				Origin:         cliTaskOrigin(os.Getenv),
			})
			if createErr == nil && stage != "" {
				if session, client, _, err := attachFlags(cmd); err == nil {
					if err := attachSession(out, store.DB(), created.ID, session, client, created.RepoPath); err != nil {
						fmt.Fprintf(out, "attach session: %v\n", err)
					}
				}
			}
		}
	}

	// 5. Output Post-Run Information & Presentation
	fmt.Fprintln(out)
	printTokenTelemetry(out, genResult)

	fmt.Fprintln(out)
	if issueResp != nil {
		printClickableLink(out, issueResp.Identifier, issueURL, assigneeName)
		fmt.Fprintln(out)
	}

	// 6. Fleet Status & Pacing Engine
	statusCmd.Run(cmd, nil)

	return nil
}

func sanitizeComment(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) >= 2 {
		if (trimmed[0] == '"' && trimmed[len(trimmed)-1] == '"') ||
			(trimmed[0] == '\'' && trimmed[len(trimmed)-1] == '\'') {
			trimmed = strings.TrimSpace(trimmed[1 : len(trimmed)-1])
		}
	}
	return trimmed
}

func renderMarkdownSummary(task ai.InferredTask, assigneeInfo ...string) string {
	tags := strings.Join(task.Labels, ", ")
	if tags == "" {
		tags = "none"
	}

	assigneeStr := task.AssigneeRole
	if len(assigneeInfo) > 0 && assigneeInfo[0] != "" {
		if len(assigneeInfo) > 1 && assigneeInfo[1] != "" {
			assigneeStr = fmt.Sprintf("%s (%s)", assigneeInfo[0], assigneeInfo[1])
		} else {
			assigneeStr = assigneeInfo[0]
		}
	}

	statusStr := task.Status
	if statusStr == "" {
		statusStr = "todo"
	}

	card := fmt.Sprintf("# %s\n\n**Organization:** %s | **Project:** %s\n**Priority:** `%s` | **Disposition:** `%s` | **Assignee Role:** `%s` | **Labels:** `%s`\n\n---\n\n%s\n",
		task.Title,
		task.Organization,
		task.Project,
		strings.ToUpper(task.Priority),
		strings.ToUpper(statusStr),
		assigneeStr,
		tags,
		task.Description,
	)

	if task.AskClarification != "" {
		card += fmt.Sprintf("\n> ⚠️ **Clarification Requested:** %s\n", task.AskClarification)
	}

	renderer, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(80),
	)
	if err != nil {
		return card
	}

	out, err := renderer.Render(card)
	if err != nil {
		return card
	}
	return out
}

func printTokenTelemetry(out io.Writer, res *ai.GenerationResult) {
	fmt.Fprintln(out, "\033[1;36m[StayPoint :: Inference Telemetry & Cost Engine]\033[0m")
	modelTag := res.Model
	if res.FallbackUsed {
		modelTag += " \033[1;33m(Fallback Downshift)\033[0m"
	} else {
		modelTag += " \033[1;32m(Primary)\033[0m"
	}
	fmt.Fprintf(out, "  • Inference Engine:        %s\n", modelTag)
	fmt.Fprintf(out, "  • Input Tokens:            %d\n", res.InputTokens)
	fmt.Fprintf(out, "  • Output Tokens:           %d\n", res.OutputTokens)
	fmt.Fprintf(out, "  • Cached Tokens:           %d\n", res.CachedTokens)
	costColor := "\033[1;32m"
	if res.Unpriced {
		costColor = "\033[1;33m"
	}
	fmt.Fprintf(out, "  • Estimated Turn Cost:     %s%s\033[0m\n", costColor, res.FormatCost())
}

func printClickableLink(out io.Writer, identifier, issueURL string, assigneeName ...string) {
	fmt.Fprintln(out, "\033[1;36m[StayPoint :: Paperclip Issue Dispatch]\033[0m")
	fmt.Fprintf(out, "  • Issue Identifier:        \033[1;32m%s\033[0m\n", identifier)
	if len(assigneeName) > 0 && assigneeName[0] != "" {
		fmt.Fprintf(out, "  • Assigned To:             \033[1;35m%s\033[0m\n", assigneeName[0])
	}
	fmt.Fprintf(out, "  • Clickable Web Link:      \033[1;34m\033[4m%s\033[0m\n", issueURL)
}
