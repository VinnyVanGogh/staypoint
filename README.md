# Staypoint (`staypoint`)

[![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?style=flat&logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Build Status](https://img.shields.io/badge/Build-Passing-brightgreen.svg)]()
[![Platform](https://img.shields.io/badge/Platform-macOS%20%7C%20Linux-lightgrey.svg)]()
[![Pure Go](https://img.shields.io/badge/CGO-0%20(Pure%20Go)-blueviolet.svg)]()
[![Statusline](https://img.shields.io/badge/Latency-%3C2ms-success.svg)]()

> **Autonomous AI Agent Ops, Quota Pacing, Safety Guardrails & Cross-AI Context Platform for Claude Code and Google Antigravity / Gemini.**

Staypoint unifies disparate AI agent tooling into a single, high-performance static Go binary. It provides real-time multi-pool rate limit pacing, zero-token context handoffs across models, ephemeral git micro-checkpoints with instant undo, compiler error condensing, loop death-spiral circuit breakers, multi-agent collision detection, per-task dollar budgets, cross-agent wire scratchpads, persistent remote SSH bridging with `tmux`, and print-ready executive ROI briefing generation ("The Boss Card") rendered in pure Go.

---

## Project Status & Operational Reality

- **Current Version**: `v0.1.0`.
- **User Base**: Single-user daily driver built for the author's multi-agent workflow. There are zero external production users.
- **Platform Support**: macOS (Darwin) and Linux on `arm64` and `amd64`. Windows is not supported due to POSIX process replacement and daemon background supervision requirements.
- **Key Architectural Limitation**: `staypoint` launches agent sessions using `syscall.Exec` on Unix systems. This replaces the running process image completely so that Claude Code or Antigravity directly owns the terminal TTY, avoiding terminal emulation layers or escape sequence bugs. The tradeoff is that the CLI process terminates upon handoff and cannot supervise the running agent. All session monitoring, failure spiral detection, file collision tracking, and token spend aggregation are decoupled and supervised asynchronously by the background daemon (`staypointd`).

## Build Provenance & Agentic Pipeline

This repository was constructed in September 2026 with heavy autonomous AI agent assistance under an intensive human review, verification, and benchmarking harness. The codebase spans approximately 3,100 lines of Go across the CLI command surface and 6,000 lines across core internal engine packages. It is maintained under strict engineering standards: 100% pure Go (zero CGO), multi-platform CI, race-detector validation, automated fuzz testing, and sub-millisecond execution budgets. The tool supervises autonomous coding agents, and it was itself authored by supervised coding agents.

---

```
                                      STAYPOINT ARCHITECTURE

          ┌────────────────────────┐                             ┌────────────────────────┐
          │      Claude Code       │                             │   Google Antigravity   │
          │  (Work & Personal Pro) │                             │    (Gemini CLI / 3P)   │
          └───────────┬────────────┘                             └───────────┬────────────┘
                      │ ~/.claude/projects/*.jsonl                           │ ~/.gemini/brain/*.jsonl
                      ▼                                                      ▼
          ┌───────────────────────────────────────────────────────────────────────────────┐
          │                          staypointd Background Daemon                         │
          │  • Event-driven file watcher (fsnotify, <19MB RAM, 0% idle CPU)               │
          │  • Agent Loop Circuit Breaker (detects 3x tool failures / 5 in 5m; chime)     │
          │  • Multi-Agent Collision Detector (live active file locks with 15m TTL)       │
          │  • Task budget auto-spend attribution (tokens -> model pricing -> USD)        │
          │  • Account attribution engine (Work vs Personal via machine_role)             │
          │  • Predictive rate-limit curve analyzer & native OS notifications             │
          └───────────────────────────────────────┬───────────────────────────────────────┘
                                                  │
                                                  ▼
          ┌───────────────────────────────────────────────────────────────────────────────┐
          │                       Local SQLite Engine (staypoint.db)                      │
          │  • Pure Go (modernc.org/sqlite, CGO_ENABLED=0), WAL mode, FTS5               │
          │  • Telemetry: 100k+ requests, model pricing catalog, token attribution cache  │
          │  • Task Governance: dollar & turn limits, spend tracking, active statuses     │
          │  • Safety State: circuit breakers, active working files, collision tracking   │
          │  • Inter-Agent Wire: peer broadcast scratchpad with TTL & unread cursors      │
          └───────┬──────────────────────┬──────────────────────┬──────────────────┬──────┘
                  │                      │                      │                  │
                  ▼                      ▼                      ▼                  ▼
      ┌──────────────────────┐┌──────────────────────┐┌────────────────┐┌──────────────────────┐
      │  Pacer, Router & UI  ││ Safety & Token Diet  ││ Context Engine ││ Persistence & Bridge │
      │ • Sub-2ms statusline ││ • Git Time Machine   ││ • Handoff (0-tk││ • Remote SSH probe   │
      │ • Tokyo Night theme  ││   (undo/redo <5ms)   ││ • User Direct. ││ • Persistent tmux    │
      │ • 4 quota pools      ││ • Error Condenser    ││   Trail (clean)││ • Pre-flight rsync   │
      │ • Dynamic waterfall  ││   (80-95% token diet)││ • Multi-turn   ││ • Exec ROI ("Boss    │
      │ • Session Pickup     ││ • Circuit Breakers   ││   Goal Drift   ││   Card") 4 PDFs via  │
      │   (where/pickup)     ││ • File Collision Lock││ • Cross-Tool   ││   chromedp           │
      └──────────────────────┘└──────────────────────┘└────────────────┘└──────────────────────┘
```

<div align="center">
  <br/>
  <a href="docs/samples/sample-executive-roi-memo.pdf">
    <img src="docs/images/boss-card-preview.png" width="700" alt="Executive ROI Briefing ('The Boss Card') Preview" style="border-radius: 8px; box-shadow: 0 4px 20px rgba(0,0,0,0.15);" />
  </a>
  <p><em>Print-ready Executive Justification Memo rendered directly from telemetry in ~1.5s via pure Go Chrome DevTools Protocol. <a href="docs/samples/sample-executive-roi-memo.pdf">Download Sample PDF</a>.</em></p>
  <br/>
</div>

---

## The Problem Staypoint Solves

Modern AI software engineers work across multiple state-of-the-art coding agents. This fragmented workflow creates distinct operational headaches:

1. **Unpredictable Rate Limit Lockouts**: 5-hour rolling windows and weekly quotas exhaust without warning, grinding engineering velocity to a halt.
2. **Context Loss & Friction During Model Switching**: When one model hits a ceiling, migrating to another requires manually explaining the repository structure, active branch, modified files, diff status, and immediate next steps.
3. **Agent Death Spirals & Burned Tokens**: Autonomous agents frequently get stuck in repetitive error loops, running the exact same failing command or tool 10 times in a row, burning thousands of tokens and exhausting hourly quotas before the developer notices.
4. **Agent Destructive Edits Without an "Undo" Button**: When an agent hallucinates or makes a broken architectural edit across 15 files, rolling back with standard `git checkout` or `git stash` clobbers uncommitted human work and requires tedious manual recovery.
5. **Multi-Agent Collision on Shared Repos**: Running Claude Code in one terminal and Google Antigravity in another often leads to both agents modifying the same files concurrently, corrupting state and causing git merge nightmares.
6. **Token Waste from Massive Compiler Dumps**: Dumping raw 500-line TypeScript build errors, Go panics, or Python tracebacks into agent prompts wastes up to 3,000 tokens per turn and causes model attention dilution.
7. **Long-Session Goal Drift**: As conversations extend past 10 to 20 turns, agents forget early user constraints, obsess over Turn 1 prompts, or diverge across multiple conflicting milestones.
8. **Multi-Account & Hardware Split**: Work repositories often reside on corporate VPNs or dedicated hardware, while personal side-projects live locally.
9. **The ROI Justification Gap**: Engineers deliver hundreds of thousands of dollars in software value using AI agents, but executives only see the monthly subscription invoice. Without empirical proof of leverage, subscription upgrades are delayed or denied.

Staypoint eliminates these pain points with a single, zero-dependency Go platform.

---

## Provider Access Policy (BYOC)

Staypoint only drives the AI CLIs you have installed yourself, signed in with your own accounts. It never pools, proxies, shares, or resells provider accounts or credentials, in any tier or configuration. Decisions and measured baselines for the native orchestrator live in [`docs/ORCHESTRATOR.md`](docs/ORCHESTRATOR.md).

## Key Features

### ⏱️ Agent Time Machine (Micro-Checkpoints & Instant Undo/Redo)

Give your autonomous agents a safety net with sub-5ms snapshotting:

- **Isolated Git Index**: Creates tree snapshots using a dedicated git index (`.git/staypoint_index`) and custom git references (`refs/staypoint/checkpoints/<session>/<id>`). It **never moves `HEAD`**, never creates commit clutter on your active branch, and leaves your branch history pristine.
- **Microsecond Snapshots**: Takes full working tree snapshots in `<5ms`, capturing untracked and modified files before risky agent operations.
- **One-Command Undo (`staypoint undo`)**: Reverts agent mistakes instantly back to the exact working tree state before the agent made changes.
- **Safe Stashing**: Automatically preserves current uncommitted modifications in a temporary stash before restoring, ensuring no work is ever lost.
- **Commands**:
  - `staypoint checkpoint [-s <session>]`: Create a micro-checkpoint.
  - `staypoint undo [-n] [-k]`: Undo the latest checkpoint (or dry-run with `-n`, keep tree dirty with `-k`).
  - `staypoint redo`: Redo reverted checkpoint.
  - `staypoint checkpoints`: List session checkpoints.

### 🥗 Zero-Token Error Condenser / Token Diet (`staypoint condense`)

Massive compiler dumps and stack traces are the #1 source of token waste in AI workflows. The error condenser reduces dumps by **80% to 95%** before agent ingestion:

- **Language-Aware Reducers**:
  - **TypeScript / JavaScript**: Groups error cascades (e.g. `TS2304`, `TS2345`), deduplicates missing imports, extracts unique root causes, and limits repeated errors.
  - **Go**: Strips standard library runtime frames from panic dumps while preserving custom code failure points, function arguments, and panicking goroutines.
  - **Python**: Collapses third-party site-package frames (`venv`, `site-packages`) and preserves the core application traceback and final exception message.
  - **Generic**: Deduplicates repeated lines with `[xN repetitions]` markers, filters noisy progress bars, and caps output cleanly.
- **CLI & Pipe Integration**:
  - `staypoint condense [file] [-l <max-lines>] [-f <format>]`
  - Pipe directly from builds: `npm run build 2>&1 | staypoint condense` or `go test ./... 2>&1 | staypoint condense`

### 🛑 Agent Loop & Death-Spiral Circuit Breaker

Prevents runaway agents from burning your entire weekly token budget on repeated failures:

- **Error Signature Hashing**: Computes normalized signatures (`tool:command:error`) of tool calls and command failures from real-time transcript streaming.
- **Trip Conditions**:
  - **3 consecutive identical tool/command failures**, OR
  - **5 failures within a sliding 5-minute window**.
- **System Alarm**: Fires an immediate macOS audible alert (`Glass` chime) and native system notification when tripped.
- **Prompt Hook Enforcement**: `staypoint hook prompt` detects tripped breakers and injects high-priority warnings into the agent context, preventing further automated execution until acknowledged.
- **CLI Inspection**:
  - `staypoint breaker list [-a]`: View active or all tripped breakers.
  - `staypoint breaker reset <session-id>`: Reset a tripped breaker after manually fixing the blocker.

### 💥 Multi-Agent Collision Detection & Live File Locks

Safely run Claude Code and Google Antigravity simultaneously on the same repository:

- **Heartbeat Session Registration**: Both agents register their active presence in `staypoint.db` with working directory metadata.
- **Live Working File Tracking**: Tracks touched files with a 15-minute sliding TTL (`agent_working_files`).
- **Prompt Hook Collision Guard**: When an agent runs a prompt or tool, `staypoint hook prompt` inspects git dirty files and cross-checks active peer sessions. If another agent recently edited the same file, it injects a prominent collision warning with the peer session ID and file list.

### 💰 Per-Task Dollar & Turn Budgets

Impose hard financial and operational boundaries on autonomous tasks:

- **Financial Limits**: Set dollar caps (`--budget <usd>`) and turn limits (`--max-turns <n>`) per task.
- **Automatic Spend Attribution**: The `staypointd` telemetry daemon monitors transcript tokens and attributes exact dollar spend (via the built-in model pricing catalog) directly to the active task in SQLite.
- **Two-Tier Budget Enforcement**:
  - **80% Budget Warning**: `staypoint hook prompt` injects an amber pacing alert into prompt context when spend reaches 80%.
  - **100% Hard Block**: At 100% budget, `staypoint hook prompt` outputs a critical budget exhaustion error to stderr and exits with **code 2**, blocking autonomous loops from continuing without explicit user approval.
- **CLI Management**:
  - `staypoint task add <name> --budget 1.50 --max-turns 20`: Create budgeted task.
  - `staypoint task budget <id> --usd 2.00 --turns 25`: Adjust budget on an active task.
  - `staypoint task list`: View spend progress bar, dollar amounts, and turn counts.
  - `staypoint task done <id>`: Mark task complete.

### 📻 Cross-Agent Live Scratchpad (`staypoint wire`)

Zero-token peer-to-peer event bus for agents collaborating across separate terminals or tools:

- **SQLite-Backed Pub/Sub**: Fast broadcast channel (`wire_messages`) with channel scoping, author attribution, and time-to-live (`ttl`) pruning.
- **Per-Consumer Read Cursors**: Tracks read progress per consumer session in `wire_cursors`.
- **Automatic Context Injection**: `staypoint hook prompt` queries for unread wire broadcasts in the current repository and injects them seamlessly into the agent's turn prompt:
  ```markdown
  [WIRE BROADCAST from claude-worker (5m ago)]: Completed database migrations in internal/db/schema.sql
  ```
- **CLI Commands**:
  - `staypoint wire post "Refactored user auth, update API endpoints" [-c <channel>] [-t 3600]`
  - `staypoint wire list [-c <channel>] [-l 10]`
  - `staypoint wire prune`: Clean expired wire messages.

### 🔄 Unified Cross-Agent Continuation & Handoff Engine

Effortlessly resume, continue, or hand off agent sessions across Claude Code and Google Antigravity:

- **Instant Native Continue (`staypoint -c` / `staypoint continue`)**:
  - Inspects the current repository and identifies the most recently updated session between Claude Code and Antigravity.
  - Automatically launches the native resume command (`claude --resume <id>` or `agy -c <id>`) for that tool.
- **Interactive Multi-Tool Picker (`staypoint -r` / `staypoint resume`)**:
  - Displays a clean numbered terminal menu of recent sessions across both Claude and Antigravity with timestamps, message counts, active durations, and conversation snippets.
  - Select any session by number to resume it immediately.
- **Zero-Token Handoff Mode (`-H, --handoff`)**:
  - Combine with continue or resume: `staypoint -c -H` or `staypoint -r -H`.
  - Instead of resuming in the original tool, it generates an authoritative cross-agent handoff prompt and copies it to the clipboard (`pbcopy` / `xclip`).
- **User Directives & Constraints Trail**:
  - Automatically filters low-signal conversational filler (`"yes"`, `"ok"`, `"continue"`, `"lgtm"`, `"sounds good"`) and strips XML metadata wrappers (`<USER_REQUEST>`, `<ADDITIONAL_METADATA>`).
  - Synthesizes substantive human guidance into an anchor trail so the target model inherits all explicit instructions and negative constraints.
- **Extended Multi-Turn Goal Drift Advisory**:
  - Detects when a resumed session has extensive history (≥ 4 user turns or ≥ 3 directives).
  - Injects a high-visibility advisory into the handoff prompt warning the destination agent that the conversation has evolved past its initial prompt, preventing it from regressing to the Turn 1 objective.
- **The 5-Anchor Handoff Formula**:
  1. Target Agent Framing (`🦴 CAVEMAN` & formatting rules).
  2. Ground-Truth Git Context (branch, status, clean diffstat).
  3. Chronological User Directives Trail (filtered high-signal constraints).
  4. Work Accomplished & Recent Commit Log (`git log -n 3 --oneline`).
  5. Immediate Next Step & Open Decision Points.
- **Repo Subdirectories & `{id}-{timestamp}.md/json` Naming**:
  - Handoffs are organized into repo-scoped subdirectories: `~/.staypoint/handoffs/<repo-slug>/{id}-{timestamp}.md` and `{id}-{timestamp}.json`.
  - For example: `~/.staypoint/handoffs/staypoint/75fd7906-20260923-175959.md` and `.json`.
  - Multiple sessions across projects never overwrite each other. `/tmp/ai-handoff.md` is updated concurrently as a convenience pointer to the latest handoff for instant clipboard access (`pbcopy`).
- **Atomic `latest.md` & `latest.json` Symlinks**:
  - Each repo subdirectory maintains atomic relative symlinks `latest.md` and `latest.json` pointing directly to the most recent handoff.
  - Commands like `staypoint handoff show` and `staypoint handoff copy` seamlessly resolve the latest handoff when session ID is omitted.
- **Manual Pinning in Retention Pruning**:
  - Retention pruning (`max_handoffs_per_repo`, default 3) implements manual pinning. When pruning excess snapshots, automated background turns (`trigger: "auto_daemon"` or `"active_session"`) are evicted first, ensuring intentional human handoffs (`trigger: "manual"`) remain pinned and protected from eviction.
- **Multi-Branch Awareness & Branch Discovery**:
  - Default listings show branch context without hiding cross-branch work, explicitly highlighting when a handoff branch differs from the active branch (e.g. `[feat/billing (current: main)]`).
  - Filter listings by branch using `staypoint handoff list -b <branch>` or `staypoint handoff search -b <branch>`.
  - Discover all git branches containing saved handoffs using `staypoint handoff branches`.
- **Proactive Context Pickup Banner**:
  - When an incoming agent starts a turn, `staypoint hook prompt` automatically checks for recent (<4h) handoffs in the current repository from previous sessions.
  - Injects a high-visibility context pickup alert into prompt context with the previous session ID, age, git branch, goal, and file path so the incoming agent can immediately adopt prior work.
- **Structured JSON Manifests (`{id}-{timestamp}.json`)**:
  - Every handoff produces a compact metadata manifest alongside the markdown file with the session ID, goal, human-readable title, branch, modified files, user turn count, directive count, and triggering event.
- **Configurable Per-Repo Retention Pruning**:
  - Automatically prunes older handoffs within each repository subdirectory based on `max_handoffs_per_repo` in `config.toml` (default 3), keeping each project clean.
- **Zero-Token Manifest Search**:
  - Agents and humans can query saved handoffs instantly using `staypoint handoff list` or `staypoint handoff search <query>`. Searching lightweight JSON manifests consumes virtually zero tokens (~50 tokens) compared to loading full transcripts.
- **Automatic Base Handoffs on Crash / Breaker / Exit**:
  - Because Staypoint handoffs are 100% programmatic (pure Go, 0 LLM tokens, <10ms runtime), the `staypointd` background daemon automatically snapshots a base handoff whenever a session crashes, hits an error, trips a circuit breaker, or gets closed (e.g. `Ctrl+C`). You always have a reliable restart point.

### 📍 Instant Context Pickup (`staypoint where` / `staypoint pickup`)

Forgot what you were doing in a repository after stepping away?

- Run `staypoint where` or `staypoint pickup` inside any project folder.
- Displays the active branch, modified files, unpushed commits, active task spend, and a chronological table of recent Claude and Antigravity sessions with resumption commands.
- Paired with the global `where-were-we` skill for agent self-grounding.

### ⚡ Sub-2ms Tokyo Night Statusline

An ultra-low-latency statusline generator designed to integrate into Claude Code (`settings.json`), Antigravity shell hooks, and tmux. Renders recessed Braille and block progress meters displaying:

- Current active model & account role (`󰛡 Gemini (Native)` vs `🪪 Claude Max`).
- Rolling 5-hour session quota consumption and exact reset time.
- Weekly quota runway and reset target.
- Dynamic routing advice with turn runway estimation.

### 🧭 Dynamic Multi-Pool Routing Waterfall

Tracks 4 quota pools concurrently:

1. **Work Claude** (Corporate subscription)
2. **Personal Claude** (Claude Max 5x pool)
3. **Gemini Native** (Google Antigravity primary daily driver)
4. **3P Claude** (Third-party Anthropic runner)

Inspects the current working directory, git origin, SSH reachability to corporate nodes, and quota headroom to instantly route each command to the optimal model.

### 🌉 Resilient Work Bridge & Remote `tmux` Persistence

Seamlessly bridges local workstations with enterprise hardware (e.g., `company-mbp`):

- **Dynamic Path Translation**: Translates local mirror paths to remote enterprise repo structures.
- **2-Second Latency Probe**: Tests SSH reachability with a fast timeout and latency benchmark.
- **Persistent `tmux` Execution**: Automatically creates or attaches to named remote `tmux` sessions (`staypoint-<repo>`). Dropping an SSH connection never kills running builds or agent tasks.
- **Pre-Flight Transcript Sync**: Runs non-blocking `rsync` pulling remote agent transcripts into local telemetry before launching.
- **Zero-Close Shell Fallback**: If the remote host is offline, falls back to local execution without closing the terminal window.

### 📄 Executive ROI Briefing Suite ("The Boss Card")

Pure Go PDF rendering engine powered by Chrome DevTools Protocol (`chromedp`). Eliminates all Node, Bun, and Playwright dependencies:

- **Work Report (`--type work`)**: "The Boss Card." Displays net engineering value delivered, requests processed, and cache efficiency. When `hourly_rate` is set, calculates hours saved; when `0.0`, highlights direct value-to-cost multipliers (e.g., `14.4x net return on upgrade`).
- **Personal Audit (`--type personal`)**: Value audit covering Claude Max usage (100k+ requests, token distributions, cost benchmarks).
- **Gemini Native Report (`--type gemini`)**: Antigravity token throughput, flash vs. pro distributions, and code review logs.
- **Combined Fleet Memo (`--type combined`)**: Unified executive overview calculating combined impact across all platforms ($14,000+ delivered value).
- **Batch Generation (`--type all`)**: Renders all 4 print-ready PDFs to `~/Desktop` with a single command.

### 🌐 Multi-Machine Fleet & Air-Gapped MDM Support

Engineered for developers who use separate hardware for work and personal engineering:

- **`machine_role = "work" | "personal" | "hybrid"`**: Enforces 100% account attribution on dedicated laptops without guessing folder paths.
- **Network Sync (`staypoint sync pull <remote>`)**: Syncs transcripts across machines over SSH or Tailscale.
- **Air-Gapped Export/Import (`staypoint sync export` & `staypoint sync import`)**: Packages telemetry into compressed `.tar.gz` bundles. Move telemetry across corporate firewalls via AirDrop, Slack, or secure thumbdrives with idempotent SQLite merging.
- **Networked Handoff (`staypoint handoff --push` & `--pull`)**: Bypasses corporate MDM blocks on macOS Universal Clipboard by transferring handoff context directly over SSH.

---

## Quick Start

### 1. Installation

#### Homebrew (macOS & Linux)

```bash
brew tap VinnyVanGogh/tap
brew install staypoint
```

#### From Source (Go 1.23+)

```bash
git clone https://github.com/VinnyVanGogh/staypoint.git
cd staypoint
go build -o ~/.local/bin/staypoint ./cmd/staypoint
go build -o ~/.local/bin/staypointd ./cmd/staypointd

# On macOS, codesign the binaries. Use a real identity (see
# `security find-identity -v -p codesigning`) so privacy permissions survive
# rebuilds; ad-hoc (`-s -`) works but re-prompts after every rebuild.
codesign -s "<identity>" -f --timestamp=none -i com.staypoint.cli ~/.local/bin/staypoint
codesign -s "<identity>" -f --timestamp=none -i com.staypoint.daemon ~/.local/bin/staypointd
# Or just run: scripts/reinstall-daemon.sh
```

`scripts/reinstall-daemon.sh` refuses to deploy a tree with uncommitted changes (untracked files included) or a HEAD that is not on `origin/main`, and refuses a binary with no `vcs.revision`. Deploy from a clean checkout of `origin/main`, such as a dedicated worktree. `--allow-dev-build` deploys anyway, prints a banner, and makes the daemon report `dev_build: true` in `/api/health` and a DEV BUILD badge in the web UI header. A daemon built with plain `go build` reports `dev_build: true` too. Every install and refused install is logged to `~/.staypoint/deploys.log`.

Verify the installation:

```bash
staypoint version
# staypoint version 0.1.0
```

#### macOS Background Daemon Setup

Install `staypointd` as a user LaunchAgent:

```bash
cat << 'EOF' > ~/Library/LaunchAgents/com.staypoint.daemon.plist
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.staypoint.daemon</string>
    <key>ProgramArguments</key>
    <array>
        <string>/opt/homebrew/bin/staypointd</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/staypointd.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/staypointd.err</string>
</dict>
</plist>
EOF

launchctl load ~/Library/LaunchAgents/com.staypoint.daemon.plist
```

### 2. Shell Integration

Add the shell evaluation hook to your `~/.zshrc` or `~/.bashrc`:

```bash
eval "$(staypoint init --shell)"
```

This registers the `ai` command wrapper, auto-routing evaluations, fast statusline rendering, and terminal-safe execution.

### 3. Web UI end-to-end tests

`scripts/ui-e2e.sh` drives the web UI in headless Chromium (Playwright) against a throwaway daemon on a temp database. It never touches `~/.staypoint` or the launchd daemon. Needs Go, Node and Python 3. See [`tests/ui/README.md`](tests/ui/README.md).

```bash
scripts/ui-e2e.sh                 # whole suite
scripts/ui-e2e.sh -g "interaction" # one area
```

---

## Configuration

Configuration is located at `~/.staypoint/config.toml` (or `~/.staypoint/config.json`):

```toml
# ==============================================================================
# STAYPOINT CONFIGURATION
# ==============================================================================

# Executive Reporting Metadata
company_name = "Company Name"
engineer_name = "Staff Engineer"

# Billable Client Rate (Hourly)
# Set to your client billing rate (e.g., 250.0).
# When set to 0.0, billable client hours are completely omitted from reports,
# and direct value-to-cost multipliers (e.g. 14.4x return) are displayed instead.
hourly_rate = 0.0

# Account Attribution
work_email = "user@example.com"
personal_email = "personal@gmail.com"

# Machine Role: "work", "personal", or "hybrid" (default)
# - "work": Treats 100% of telemetry on this machine as work activity.
# - "personal": Treats 100% of telemetry on this machine as personal activity.
# - "hybrid": Uses folder path matching and repo configurations.
machine_role = "hybrid"

# Work Bridge & Remote Node
work_repo_root = "~/Documents/dev/company"
remote_host = "company-mbp"

# Session Handoff Retention
# Maximum number of recent session handoffs and manifests to keep per repository
max_handoffs_per_repo = 3

# Provider CLI binaries used by `staypoint adapter` (optional)
# Empty = resolve `claude` / `agy` on PATH, then ~/.local/bin. Staypoint bridge
# shims and the staypoint binary itself are skipped to avoid recursion.
# Env vars STAYPOINT_CLAUDE_BIN / STAYPOINT_AGY_BIN take precedence.
claude_bin = ""
agy_bin = ""
```

Run `staypoint adapter probe` to see which CLI binaries the adapter resolves and their versions. It warns when an installed CLI's major version has not been verified against the adapter's golden stream fixtures (upstream flag or stream-schema drift).

---

## CLI Command Reference

### Primary Interactive Launcher & Resumption (`staypoint`)

```bash
# Automatically launches optimal AI (remote Claude tmux, local Claude, or Antigravity agy)
staypoint

# Pass prompts or flags directly to the routed AI
staypoint "implement new authentication flow"

# Force routing flags
staypoint -C, --claude        # Force route to Claude Code
staypoint -G, --gemini        # Force route to Antigravity Gemini (agy)
staypoint --dry-run           # Preview routed target, model, and bridge status without executing
staypoint --status            # Quick display of fleet status & quota table

# Instant Session Continuation & Picker
staypoint -c, --continue      # Continue latest session for current repository (auto-detects Claude or agy)
staypoint -r, --resume        # Interactive cross-tool session picker (Claude + Antigravity)
staypoint -c -H, --handoff    # Copy cross-agent continuation prompt to clipboard instead of resuming
staypoint -r -H               # Pick a past session and generate a handoff prompt for another agent
```

### Agent Time Machine (Micro-Checkpoints & Instant Undo)

```bash
# Snapshot current working tree into isolated git ref (<5ms)
staypoint checkpoint

# Snapshot with custom session label
staypoint checkpoint -s feature-auth

# Undo agent changes back to previous checkpoint
staypoint undo

# Preview what undo would revert without touching the working tree
staypoint undo --dry-run / -n

# Undo working tree changes but keep staged index dirty
staypoint undo -k

# Redo previously reverted checkpoint
staypoint redo

# List available checkpoints for current session/repo
staypoint checkpoints
```

### Zero-Token Error Condenser / Token Diet

```bash
# Condense compiler errors or stack traces from a log file
staypoint condense build-error.log

# Pipe directly from build tools (condenses by 80-95% before agent consumption)
npm run build 2>&1 | staypoint condense
go test ./... 2>&1 | staypoint condense
pytest 2>&1 | staypoint condense

# Force format parser and customize max output lines
staypoint condense -f typescript -l 25 ts-errors.log
staypoint condense -f golang go-panic.log
staypoint condense -f python py-traceback.log
```

### Circuit Breakers & Collision Detection

```bash
# List all active tripped circuit breakers
staypoint breaker list

# List all circuit breakers (including resolved/historic)
staypoint breaker list -a

# Reset a tripped circuit breaker for a session
staypoint breaker reset <session-id>
```

### Per-Task Dollar & Turn Budgets

```bash
# Add a new task with dollar budget and turn limit
staypoint task add "Migrate DB schema" --budget 2.50 --max-turns 30

# Update budget on an existing task
staypoint task budget <task-id> --usd 4.00 --turns 50

# List active tasks with spend meters, token usage, and turn counts
staypoint task list

# List all tasks including completed
staypoint task list --all

# Mark task as completed
staypoint task done <task-id>
```

### Cross-Agent Live Scratchpad (`staypoint wire`)

```bash
# Post a broadcast message to other agents in the repo
staypoint wire post "Added new migration in internal/db/002_auth.sql"

# Post with custom channel and TTL (in seconds)
staypoint wire post "Reviewing auth controller" -c reviews -t 7200

# List recent broadcast messages
staypoint wire list
staypoint wire list -c reviews -l 20

# Clean expired wire messages from database
staypoint wire prune
```

### Context Pickup & Handoff

```bash
# Inspect current repo context, active task, and recent session history
staypoint where
staypoint pickup

# Generate handoff prompt to Gemini and copy to clipboard (saves to ~/.staypoint/handoffs/)
staypoint handoff --to gemini

# Generate handoff prompt with specific next step directive
staypoint handoff --to claude --step "Implement modernc.org/sqlite schema migration"

# List saved session handoffs and compact metadata manifests for current repository
staypoint handoff list
staypoint handoffs

# Filter handoffs by git branch
staypoint handoff list -b feat/billing

# Discover all branches in the repository that have saved handoffs
staypoint handoff branches

# List handoffs across all repositories or output JSON
staypoint handoff list --all --limit 20
staypoint handoff list --json

# Zero-token manifest search (search by goal, title, session ID, or branch)
staypoint handoff search "authentication"
staypoint handoff search "stripe" -b feat/billing

# View or copy the latest saved handoff prompt (resolves latest.md)
staypoint handoff show
staypoint handoff copy

# View or copy a specific saved handoff prompt by session ID
staypoint handoff show <session-id>
staypoint handoff copy <session-id>

# Push handoff context directly to remote machine and remote clipboard
staypoint handoff --push company-mbp

# Pull handoff context from remote machine into local clipboard
staypoint handoff --pull company-mbp

# Claude / Antigravity prompt hook (monitors quota, breakers, collisions, wire, budgets)
staypoint hook prompt
```

### Pacing & Status

```bash
# Display live fleet status, quota gauges, active tasks, and routing advice
staypoint status

# Render instantaneous statusline (<2ms) for prompt integration
staypoint statusline

# Get routing recommendation for current directory (human-readable)
staypoint route

# Output shell-evaluable routing recommendation
staypoint route --eval
```

### Reporting & Executive ROI ("The Boss Card")

```bash
# Generate Work Justification Memo ("The Boss Card") PDF
staypoint report --pdf --type work

# Filter by date range (supports exact dates or relative ranges like 7d, 30d)
staypoint report --pdf --type work --since 2026-08-01 --until 2026-09-01
staypoint report --pdf --type combined --since 30d

# Generate Personal Claude Code Value Audit PDF
staypoint report --pdf --type personal

# Generate Antigravity & Gemini Native Report PDF
staypoint report --pdf --type gemini

# Generate Combined Multi-AI Fleet Executive Report PDF
staypoint report --pdf --type combined

# Batch generate all 4 executive PDF reports to ~/Desktop
staypoint report --pdf --type all

# Specify a custom destination path
staypoint report --pdf --type work -o ~/Documents/Boss-Card-Q1.pdf
```

### Remote Bridge & Sessions

```bash
# Check remote SSH connectivity, latency, and path translation
staypoint bridge check ~/Documents/dev/company/partner-center-api

# Launch interactive Claude session in persistent remote tmux
staypoint bridge launch ~/Documents/dev/company/partner-center-api

# Execute remote build command inside remote tmux session
staypoint bridge launch ~/Documents/dev/company/partner-center-api go test ./...
```

### Multi-Machine Synchronization

```bash
# Pull transcripts from remote host over SSH/Tailscale & ingest into local DB
staypoint sync pull company-mbp

# Export local telemetry database into portable compressed bundle
staypoint sync export -o ~/Desktop/work-telemetry.tar.gz

# Import telemetry bundle into local database (idempotent)
staypoint sync import ~/Desktop/work-telemetry.tar.gz
```

---

## Statusline Integration

Staypoint renders a 5-line recessed Tokyo Night terminal widget in `<2ms` with zero CPU overhead. It dynamically detects whether you are active in Claude Code or Antigravity and switches badges, account indicators, and runway advice in real time.

### 1. Claude Code Integration

Point `~/.claude/settings.json` statusline command to `staypoint statusline`:

```json
{
  "statusline": {
    "command": "staypoint statusline"
  }
}
```

### 2. Google Antigravity / Gemini CLI Integration

When using Antigravity (`agy`), `staypoint statusline` is automatically displayed before agent execution via the shell integration:

```bash
# In ~/.zshrc or ~/.bashrc:
eval "$(staypoint init --shell)"

# Or alias directly for standalone agy usage:
alias agy="staypoint statusline && agy"
```

The statusline dynamically displays:

- **`󰛡 Gemini (Native)`** when routing to Antigravity (`gemini-3.8-flash-high` or `gemini-3.1-pro-high`).
- **`🪪 Claude Max / Pro`** when routing to Anthropic Claude models.

### 3. tmux Statusbar Integration

Display live agent fleet pacing directly in your tmux status bar. Add to `~/.tmux.conf`:

```tmux
set -g status-right "#(staypoint statusline)"
set -g status-interval 10
```

### Visual Output (Tokyo Night Palette)

```
󰛡 Gemini (Native) │ 🪪 personal@gmail.com │ ⚡ staypoint:active
📁 staypoint │ 🐙 main │ 🦴 CAVEMAN
▏███████████████░░░░░▕ session:75% ~25% left @4:12pm
▏████████████████░░░░▕ weekly:81% ~19% left @tue 8:11pm
plan: route ▸ gemini-3.8-flash-high (agy) · fallback: claude-sonnet-4-6 · runway: 16 turns
```

Benchmark: **1.8ms** execution time (compiled pure Go, sub-process safe).

---

## API End-to-End Suite (Postman / newman)

`tests/api/` holds a Postman collection that exercises every route of the daemon HTTP API: happy paths, edge cases (missing fields, unknown IDs, invalid transitions, duplicates, ordering, unauthenticated calls) and one full task workflow from create to audit.

**Run it headless** (needs Go, Python 3, and `newman` or Node's `npx`):

```bash
scripts/api-e2e.sh                      # whole suite; exits non-zero on any failure
scripts/api-e2e.sh --folder "01 Tasks"  # one folder (repeatable)
E2E_JUNIT=api-e2e.xml scripts/api-e2e.sh  # also write a JUnit report
```

The script builds `cmd/staypoint-apitest-server`, starts it on a fresh SQLite file in a temp dir with `HOME` re-pointed there, and starts a Paperclip stub for the `/api/fleet/*` proxies. It never opens `~/.staypoint/staypoint.db`, never talks to the real `staypointd` socket, and never calls the live Paperclip API.

**Open it in Postman:** run `scripts/api-e2e.sh --serve`. It starts the same throwaway daemon and stub and prints `baseUrl` and `token`. Import `tests/api/staypoint.postman_collection.json` and `tests/api/staypoint.postman_environment.json`, paste the two values into the environment, and run the folders in order (later folders reuse IDs saved by earlier ones). Ctrl-C stops the daemon and deletes its temp DB. Folder 13 (SSE) is for manual use in Postman; the headless run checks SSE with `curl`.

Tests named `[STA-nnn] ...` fail until that daemon bug is fixed (STA-355 to STA-358 at the time of writing).

The collection is generated: edit `tests/api/build_collection.py`, then run `python3 tests/api/build_collection.py`. The script fails if the JSON is stale or if a route registered in `internal/server` has no request in the collection.

## Technical Architecture & Design Decisions

### 1. Pure Go SQLite (Zero CGO)

Staypoint uses `modernc.org/sqlite` rather than `mattn/go-sqlite3`. This allows compiling the binary with `CGO_ENABLED=0`, producing 100% statically linked binaries with zero dynamic library dependencies while retaining full SQLite WAL mode, memory concurrency, and FTS5 full-text search.

### 2. Isolated Git Index for Micro-Checkpoints

`staypoint checkpoint` never interferes with your working branch or commit history. By directing git plumbing commands (`git write-tree`, `git commit-tree`, `git update-ref`) through an isolated index environment (`GIT_INDEX_FILE=.git/staypoint_index`), Staypoint snapshots unstaged and staged files in `<5ms` under `refs/staypoint/checkpoints/` without touching `HEAD`.

### 3. Native Chrome DevTools Protocol (`chromedp`)

Executive PDF generation communicates directly with the local Google Chrome binary (`/Applications/Google Chrome.app`) over WebSockets via Chrome DevTools Protocol. This eliminates the need for Node.js, Bun, Playwright, or Puppeteer runtimes. Reports are rendered in ~1.5 seconds.

### 4. Non-Blocking Tail Ingestion & Event Daemon

The `staypointd` daemon maintains an event-driven file watcher (`fsnotify`) with byte-offset cursors stored in `~/.staypoint/ingest-cursors.json`. Transcripts are scanned using 4MB line buffers, extracting token metrics, evaluating circuit breakers, tracking working file touches, and attributing spend with zero noticeable CPU overhead (<0.1% CPU, 19MB RAM).

### 5. Idempotent Data Model

Every telemetry record generates a deterministic SHA-256 idempotency key based on its raw transcript payload. Syncing transcripts repeatedly or importing bundles across machines will never duplicate token accounting or financial numbers.

---

## Platform Support & Cross-Compilation

Staypoint officially supports macOS (Darwin) and Linux across ARM64 and AMD64 architectures. Release binaries are cross-compiled via `.goreleaser.yaml` and automated GitHub Actions (`.github/workflows/ci.yml`):

Supported build targets:

- `darwin/arm64` (Apple Silicon M1/M2/M3/M4)
- `darwin/amd64` (Intel Mac)
- `linux/amd64` (Standard Linux)
- `linux/arm64` (ARM Linux / Raspberry Pi / Graviton)

---

## Privacy & Local-First Manifesto

- **100% Local**: All SQLite databases, telemetry records, cursors, checkpoints, and reports reside on your machine in `~/.staypoint/`.
- **Zero Telemetry Phone-Home**: Staypoint makes zero outbound network requests to third-party telemetry services, tracking servers, or analytics endpoints.
- **Secure Network Bridging**: Network operations only occur across user-configured SSH keys or Tailscale nodes.

---

## License

Copyright © 2026 Vince Vasile.

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for details.

