package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

const Schema = `
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS accounts (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    account_key   TEXT UNIQUE,
    email_domain  TEXT UNIQUE,
    role          TEXT NOT NULL CHECK (role IN ('work', 'personal', 'other')),
    label         TEXT NOT NULL,
    plan_tier     TEXT NOT NULL,
    notes         TEXT,
    registered_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS quota_windows (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    pool_key      TEXT NOT NULL,
    window_type   TEXT NOT NULL CHECK (window_type IN ('rolling_5h', 'weekly_7d', 'monthly')),
    used_percent  REAL NOT NULL DEFAULT 0.0,
    remaining_pct REAL NOT NULL DEFAULT 100.0,
    is_locked     INTEGER NOT NULL DEFAULT 0,
    resets_at     TEXT,
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (pool_key, window_type)
);

-- Per-provider fetch bookkeeping: throttle (next_attempt_at) and last outcome.
CREATE TABLE IF NOT EXISTS quota_fetch_state (
    provider        TEXT PRIMARY KEY,
    last_attempt_at TEXT NOT NULL,
    next_attempt_at TEXT NOT NULL,
    last_success_at TEXT,
    last_status     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    repo_path      TEXT NOT NULL,
    git_branch     TEXT,
    status         TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'done', 'soft_deleted')),
    account_role   TEXT NOT NULL DEFAULT 'work',
    max_budget_usd REAL NOT NULL DEFAULT 0.0,
    max_turns      INTEGER NOT NULL DEFAULT 0,
    spent_tokens   INTEGER NOT NULL DEFAULT 0,
    spent_usd      REAL NOT NULL DEFAULT 0.0,
    spent_turns    INTEGER NOT NULL DEFAULT 0,
    organization   TEXT,
    project        TEXT,
    is_blocked     INTEGER NOT NULL DEFAULT 0,
    block_reason   TEXT,
    parent_id      TEXT REFERENCES tasks(id),
    execution_stage TEXT NOT NULL DEFAULT 'todo',
    checkout_run_id TEXT,
    checkout_agent_id TEXT,
    assignee_agent_id TEXT,
    work_kind      TEXT NOT NULL DEFAULT 'coding',
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    deleted_at     TEXT
);

CREATE TABLE IF NOT EXISTS task_comments (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id     TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    author      TEXT NOT NULL DEFAULT 'system',
    message     TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS task_relations (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id       TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    blocks_id     TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    rationale     TEXT,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE(task_id, blocks_id)
);

CREATE TABLE IF NOT EXISTS task_documents (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id       TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    doc_key       TEXT NOT NULL,
    version       INTEGER NOT NULL DEFAULT 1,
    content       TEXT NOT NULL,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE(task_id, doc_key, version)
);

CREATE TABLE IF NOT EXISTS task_interactions (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id       TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    interaction_kind TEXT NOT NULL,
    payload       TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending',
    idempotency_key TEXT,
    supersede_on_comment INTEGER NOT NULL DEFAULT 1,
    response      TEXT,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    resolved_at   TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_task_interactions_idempotency ON task_interactions(task_id, idempotency_key) WHERE idempotency_key IS NOT NULL AND idempotency_key != '';

CREATE TABLE IF NOT EXISTS task_work_products (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id       TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    product_type  TEXT NOT NULL CHECK (product_type IN ('pull_request', 'commit', 'branch', 'workspace_file')),
    reference     TEXT NOT NULL,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS activity_log (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id       TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    event_type    TEXT NOT NULL,
    details       TEXT,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS wire_messages (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    channel     TEXT NOT NULL DEFAULT 'global',
    author      TEXT NOT NULL,
    repo_path   TEXT,
    content     TEXT NOT NULL,
    ttl_seconds INTEGER NOT NULL DEFAULT 86400,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    expires_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_wire_messages_channel_expires ON wire_messages (channel, expires_at);
CREATE INDEX IF NOT EXISTS idx_wire_messages_repo_expires ON wire_messages (repo_path, expires_at);
CREATE INDEX IF NOT EXISTS idx_wire_messages_id_expires ON wire_messages (id, expires_at);

CREATE TABLE IF NOT EXISTS wire_cursors (
    consumer_key TEXT PRIMARY KEY,
    last_read_id INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS agent_sessions (
    id                TEXT PRIMARY KEY,
    agent_type        TEXT NOT NULL CHECK (agent_type IN ('claude', 'gemini', 'codex', 'other')),
    repo_path         TEXT NOT NULL,
    git_branch        TEXT NOT NULL DEFAULT 'main',
    pid               INTEGER,
    hostname          TEXT NOT NULL DEFAULT 'local',
    status            TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'idle', 'closed')),
    started_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_heartbeat_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    metadata_json     TEXT
);

CREATE INDEX IF NOT EXISTS idx_agent_sessions_repo ON agent_sessions (repo_path, status, last_heartbeat_at);

CREATE TABLE IF NOT EXISTS agent_working_files (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id        TEXT NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
    repo_path         TEXT NOT NULL,
    file_path         TEXT NOT NULL,
    access_type       TEXT NOT NULL CHECK (access_type IN ('read', 'write', 'lock')),
    first_touched_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_touched_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    expires_at        TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_working_files_sess_path ON agent_working_files (session_id, file_path);
CREATE INDEX IF NOT EXISTS idx_agent_working_files_repo_path ON agent_working_files (repo_path, expires_at);

CREATE TABLE IF NOT EXISTS agent_circuit_breakers (
    session_id        TEXT PRIMARY KEY,
    repo_path         TEXT NOT NULL,
    agent_type        TEXT NOT NULL,
    is_tripped        INTEGER NOT NULL DEFAULT 0,
    trip_count        INTEGER NOT NULL DEFAULT 0,
    failure_signature TEXT,
    failing_tool      TEXT,
    failing_command   TEXT,
    last_error        TEXT,
    tripped_at        TEXT,
    cleared_at        TEXT
);

CREATE INDEX IF NOT EXISTS idx_circuit_breakers_active ON agent_circuit_breakers (repo_path, is_tripped);

CREATE TABLE IF NOT EXISTS checklist_items (
    id          TEXT PRIMARY KEY,
    sprint      TEXT NOT NULL DEFAULT 'STA-168',
    section     TEXT NOT NULL,
    title       TEXT NOT NULL,
    description TEXT,
    how_to_test TEXT,
    contract    TEXT,
    status      TEXT NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending','pass','fail','skip','not_done')),
    notes       TEXT,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_checklist_sprint_section ON checklist_items (sprint, section);

CREATE TABLE IF NOT EXISTS checklist_history (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    item_id     TEXT NOT NULL REFERENCES checklist_items(id) ON DELETE CASCADE,
    status      TEXT NOT NULL,
    notes       TEXT,
    changed_by  TEXT NOT NULL DEFAULT 'user',
    changed_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_checklist_history_item ON checklist_history (item_id, id DESC);
CREATE TABLE IF NOT EXISTS run_steps (
    id          TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL,
    task_id     TEXT REFERENCES tasks(id),
    seq         INTEGER NOT NULL DEFAULT 0,
    parent_seq  INTEGER,
    kind        TEXT NOT NULL DEFAULT '',
    title       TEXT NOT NULL DEFAULT '',
    body        TEXT,
    command     TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT '',
    started_at  TEXT,
    ended_at    TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_run_steps_run ON run_steps (run_id, seq);
CREATE INDEX IF NOT EXISTS idx_run_steps_task ON run_steps (task_id, seq);

CREATE TABLE IF NOT EXISTS run_errors (
    id           TEXT PRIMARY KEY,
    run_id       TEXT NOT NULL,
    task_id      TEXT REFERENCES tasks(id) ON DELETE CASCADE,
    turn         INTEGER NOT NULL DEFAULT 0,
    exit_code    INTEGER NOT NULL DEFAULT 0,
    stderr_tail  TEXT NOT NULL DEFAULT '',
    duration_ms  INTEGER NOT NULL DEFAULT 0,
    model        TEXT NOT NULL DEFAULT '',
    adapter      TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_run_errors_task ON run_errors (task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_run_errors_run ON run_errors (run_id);

CREATE TABLE IF NOT EXISTS run_control (
    task_id          TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    pause_after_step INTEGER NOT NULL DEFAULT 0,
    stop_requested   INTEGER NOT NULL DEFAULT 0,
    updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS run_pending_messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    message    TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_run_pending_messages_task ON run_pending_messages (task_id, id);

CREATE TABLE IF NOT EXISTS ship_review_cards (
    id                TEXT PRIMARY KEY,
    task_id           TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    branch            TEXT NOT NULL,
    head_sha          TEXT NOT NULL,
    test_steps_json   TEXT NOT NULL DEFAULT '[]',
    dev_url           TEXT NOT NULL DEFAULT '',
    dev_pid           INTEGER NOT NULL DEFAULT 0,
    status            TEXT NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending','approved','sent_back','rejected')),
    approved_sha      TEXT,
    main_sha          TEXT,
    send_back_comment TEXT,
    reject_comment    TEXT,
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_ship_review_task ON ship_review_cards (task_id, created_at DESC);

CREATE TABLE IF NOT EXISTS project_dev_configs (
    repo_path        TEXT PRIMARY KEY,
    dev_command      TEXT NOT NULL DEFAULT '',
    dev_url          TEXT NOT NULL DEFAULT '',
    setup_steps_json TEXT NOT NULL DEFAULT '[]',
    updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
` + ChatSchema

const ChatSchema = `
CREATE TABLE IF NOT EXISTS chat_sessions (
    id            TEXT PRIMARY KEY,
    title         TEXT NOT NULL DEFAULT '',
    repo_path     TEXT,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    metadata_json TEXT
);

CREATE INDEX IF NOT EXISTS idx_chat_sessions_updated ON chat_sessions (updated_at DESC);

CREATE TABLE IF NOT EXISTS chat_messages (
    id            TEXT PRIMARY KEY,
    session_id    TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    sequence_num  INTEGER NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('system', 'user', 'assistant', 'tool')),
    content       TEXT NOT NULL DEFAULT '',
    token_count   INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    metadata_json TEXT,
    UNIQUE (session_id, sequence_num)
);

CREATE INDEX IF NOT EXISTS idx_chat_messages_session ON chat_messages (session_id, sequence_num);

CREATE TABLE IF NOT EXISTS chat_tool_calls (
    id            TEXT PRIMARY KEY,
    message_id    TEXT NOT NULL REFERENCES chat_messages(id) ON DELETE CASCADE,
    sequence_num  INTEGER NOT NULL DEFAULT 0,
    name          TEXT NOT NULL,
    arguments     TEXT NOT NULL DEFAULT '{}',
    result        TEXT,
    is_error      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_chat_tool_calls_message ON chat_tool_calls (message_id, sequence_num);

CREATE TABLE IF NOT EXISTS session_provider_handles (
    session_id    TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    provider      TEXT NOT NULL,
    handle        TEXT NOT NULL,
    model         TEXT,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    metadata_json TEXT,
    PRIMARY KEY (session_id, provider)
);

CREATE INDEX IF NOT EXISTS idx_session_provider_handles_lookup ON session_provider_handles (provider, handle);
`

type Store struct {
	db *sql.DB
}

// migrateLegacyQuotaWindows drops the pre-T3 quota_windows table, which held
// one row per pool (pool_key UNIQUE) and could not store both windows. Nothing
// wrote to it before T3, so there is no data to carry over.
func migrateLegacyQuotaWindows(conn *sql.DB) error {
	var ddl string
	if err := conn.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='quota_windows'").Scan(&ddl); err != nil {
		return nil
	}
	if strings.Contains(ddl, "UNIQUE (pool_key, window_type)") {
		return nil
	}
	if _, err := conn.Exec("DROP TABLE quota_windows"); err != nil {
		return err
	}
	_, err := conn.Exec(Schema)
	return err
}

func migrateSchemaTasksCols(conn *sql.DB) error {
	if err := migrateLegacyQuotaWindows(conn); err != nil {
		return err
	}
	rows, err := conn.Query("PRAGMA table_info(tasks);")
	if err != nil {
		return err
	}
	defer rows.Close()

	existingCols := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dfltValue interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err == nil {
			existingCols[name] = true
		} else {
			return fmt.Errorf("failed to scan table_info for column: %v", err)
		}
	}

	cols := []struct {
		name string
		def  string
	}{
		{"max_budget_usd", "REAL NOT NULL DEFAULT 0.0"},
		{"max_turns", "INTEGER NOT NULL DEFAULT 0"},
		{"spent_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"spent_usd", "REAL NOT NULL DEFAULT 0.0"},
		{"spent_turns", "INTEGER NOT NULL DEFAULT 0"},
		{"organization", "TEXT"},
		{"project", "TEXT"},
		{"is_blocked", "INTEGER NOT NULL DEFAULT 0"},
		{"block_reason", "TEXT"},
	}

	for _, c := range cols {
		if !existingCols[c.name] {
			if _, err := conn.Exec(fmt.Sprintf("ALTER TABLE tasks ADD COLUMN %s %s;", c.name, c.def)); err != nil {
				return err
			}
		}
	}
	return nil
}

type Migration struct {
	Version int
	Name    string
	Up      func(*sql.DB) error
}

var Migrations = []Migration{
	{
		Version: 1,
		Name:    "baseline",
		Up: func(conn *sql.DB) error {
			if err := migrateLegacyQuotaWindows(conn); err != nil {
				return err
			}
			if _, err := conn.Exec(Schema); err != nil {
				return err
			}
			return migrateSchemaTasksCols(conn)
		},
	},
	{
		Version: 2,
		Name:    "unified_conversation_store",
		Up: func(conn *sql.DB) error {
			_, err := conn.Exec(ChatSchema)
			return err
		},
	},
	{
		Version: 3,
		Name:    "task_graph_and_activity",
		Up: func(conn *sql.DB) error {
			cols := []struct {
				name string
				def  string
			}{
				{"parent_id", "TEXT REFERENCES tasks(id)"},
				{"execution_stage", "TEXT NOT NULL DEFAULT 'todo'"},
				{"checkout_run_id", "TEXT"},
				{"checkout_agent_id", "TEXT"},
			}
			for _, c := range cols {
				if _, err := conn.Exec(fmt.Sprintf("ALTER TABLE tasks ADD COLUMN %s %s;", c.name, c.def)); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			queries := []string{
				"CREATE TABLE IF NOT EXISTS task_relations (id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, blocks_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')), UNIQUE(task_id, blocks_id));",
				"CREATE TABLE IF NOT EXISTS task_documents (id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, doc_key TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 1, content TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')), UNIQUE(task_id, doc_key, version));",
				"CREATE TABLE IF NOT EXISTS task_interactions (id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, interaction_kind TEXT NOT NULL, payload TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')), resolved_at TEXT);",
				"CREATE TABLE IF NOT EXISTS task_work_products (id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, product_type TEXT NOT NULL CHECK (product_type IN ('pull_request', 'commit', 'branch', 'workspace_file')), reference TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')));",
				"CREATE TABLE IF NOT EXISTS activity_log (id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, event_type TEXT NOT NULL, details TEXT, created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')));",
			}
			for _, q := range queries {
				if _, err := conn.Exec(q); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 4,
		Name:    "interaction_cards_and_locking",
		Up: func(conn *sql.DB) error {
			cols := []struct {
				name string
				def  string
			}{
				{"idempotency_key", "TEXT"},
				{"supersede_on_comment", "INTEGER NOT NULL DEFAULT 1"},
				{"response", "TEXT"},
			}
			for _, c := range cols {
				if _, err := conn.Exec(fmt.Sprintf("ALTER TABLE task_interactions ADD COLUMN %s %s;", c.name, c.def)); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			_, err := conn.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_task_interactions_idempotency ON task_interactions(task_id, idempotency_key) WHERE idempotency_key IS NOT NULL AND idempotency_key != '';`)
			return err
		},
	},
	{
		Version: 5,
		Name:    "task_governance_engine",
		Up: func(conn *sql.DB) error {
			queries := []string{
				`CREATE TABLE IF NOT EXISTS task_governance (
					task_id            TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
					approval_threshold INTEGER NOT NULL DEFAULT 1,
					require_review     INTEGER NOT NULL DEFAULT 0,
					watchdog_agent_id  TEXT,
					watchdog_prompt    TEXT,
					created_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
					updated_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
				);`,
				`CREATE TABLE IF NOT EXISTS task_reviewers (
					id            INTEGER PRIMARY KEY AUTOINCREMENT,
					task_id       TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
					reviewer_id   TEXT NOT NULL,
					reviewer_type TEXT NOT NULL DEFAULT 'agent',
					assigned_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
					UNIQUE(task_id, reviewer_id)
				);`,
				`CREATE TABLE IF NOT EXISTS task_approvers (
					id            INTEGER PRIMARY KEY AUTOINCREMENT,
					task_id       TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
					approver_id   TEXT NOT NULL,
					approver_type TEXT NOT NULL DEFAULT 'agent',
					assigned_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
					UNIQUE(task_id, approver_id)
				);`,
				`CREATE TABLE IF NOT EXISTS task_approval_votes (
					id          INTEGER PRIMARY KEY AUTOINCREMENT,
					task_id     TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
					approver_id TEXT NOT NULL,
					vote        TEXT NOT NULL CHECK (vote IN ('approved', 'rejected', 'abstain')),
					reason      TEXT,
					voted_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
					UNIQUE(task_id, approver_id)
				);`,
				`CREATE TABLE IF NOT EXISTS task_review_decisions (
					id          INTEGER PRIMARY KEY AUTOINCREMENT,
					task_id     TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
					reviewer_id TEXT NOT NULL,
					decision    TEXT NOT NULL CHECK (decision IN ('approved', 'rejected', 'changes_requested')),
					notes       TEXT,
					decided_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
					UNIQUE(task_id, reviewer_id)
				);`,
				`CREATE TABLE IF NOT EXISTS task_watchdog_evals (
					id            INTEGER PRIMARY KEY AUTOINCREMENT,
					task_id       TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
					trigger_event TEXT NOT NULL,
					passed        INTEGER NOT NULL DEFAULT 0,
					verdict       TEXT NOT NULL,
					evaluated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
				);`,
				`CREATE TABLE IF NOT EXISTS governance_audit_log (
					id         INTEGER PRIMARY KEY AUTOINCREMENT,
					task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
					actor_id   TEXT NOT NULL,
					event_type TEXT NOT NULL,
					from_state TEXT,
					to_state   TEXT,
					payload    TEXT,
					created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
				);`,
				`CREATE INDEX IF NOT EXISTS idx_governance_audit_task ON governance_audit_log(task_id, created_at DESC);`,
				`CREATE INDEX IF NOT EXISTS idx_watchdog_evals_task ON task_watchdog_evals(task_id, evaluated_at DESC);`,
			}
			for _, q := range queries {
				if _, err := conn.Exec(q); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 6,
		Name:    "telemetry_queue",
		Up: func(conn *sql.DB) error {
			queries := []string{
				`CREATE TABLE IF NOT EXISTS telemetry_queue (
					id          INTEGER PRIMARY KEY AUTOINCREMENT,
					event_name  TEXT NOT NULL,
					properties  TEXT NOT NULL,
					distinct_id TEXT NOT NULL,
					timestamp   TEXT NOT NULL,
					retry_count INTEGER NOT NULL DEFAULT 0,
					status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'failed')),
					created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
				);`,
				`CREATE INDEX IF NOT EXISTS idx_telemetry_queue_status ON telemetry_queue (status, retry_count, id);`,
			}
			for _, q := range queries {
				if _, err := conn.Exec(q); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 7,
		Name:    "checklist",
		Up: func(conn *sql.DB) error {
			queries := []string{
				`CREATE TABLE IF NOT EXISTS checklist_items (
					id          TEXT PRIMARY KEY,
					sprint      TEXT NOT NULL DEFAULT 'STA-168',
					section     TEXT NOT NULL,
					title       TEXT NOT NULL,
					description TEXT,
					how_to_test TEXT,
					status      TEXT NOT NULL DEFAULT 'pending'
					            CHECK (status IN ('pending','pass','fail','skip','not_done')),
					notes       TEXT,
					version     INTEGER NOT NULL DEFAULT 1,
					created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
					updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
				);`,
				`CREATE INDEX IF NOT EXISTS idx_checklist_sprint_section
				    ON checklist_items (sprint, section);`,
				`CREATE TABLE IF NOT EXISTS checklist_history (
					id          INTEGER PRIMARY KEY AUTOINCREMENT,
					item_id     TEXT NOT NULL REFERENCES checklist_items(id) ON DELETE CASCADE,
					status      TEXT NOT NULL,
					notes       TEXT,
					changed_by  TEXT NOT NULL DEFAULT 'user',
					changed_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
				);`,
				`CREATE INDEX IF NOT EXISTS idx_checklist_history_item
				    ON checklist_history (item_id, id DESC);`,
			}
			for _, q := range queries {
				if _, err := conn.Exec(q); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 8,
		Name:    "blocker_graph_and_rationales",
		Up: func(conn *sql.DB) error {
			queries := []string{
				`ALTER TABLE task_relations ADD COLUMN rationale TEXT;`,
				`CREATE INDEX IF NOT EXISTS idx_task_relations_blocks ON task_relations (blocks_id);`,
				`CREATE INDEX IF NOT EXISTS idx_task_relations_task ON task_relations (task_id);`,
			}
			for _, q := range queries {
				if _, err := conn.Exec(q); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 9,
		Name:    "checklist_contract",
		Up: func(conn *sql.DB) error {
			// Add contract column if not present
			var count int
			_ = conn.QueryRow("SELECT COUNT(*) FROM pragma_table_info('checklist_items') WHERE name='contract'").Scan(&count)
			if count == 0 {
				_, _ = conn.Exec("ALTER TABLE checklist_items ADD COLUMN contract TEXT;")
			}
			return nil
		},
	},
	{
		Version: 10,
		Name:    "run_steps",
		Up: func(conn *sql.DB) error {
			queries := []string{
				`CREATE TABLE IF NOT EXISTS run_steps (
					id          TEXT PRIMARY KEY,
					run_id      TEXT NOT NULL,
					task_id     TEXT REFERENCES tasks(id),
					seq         INTEGER NOT NULL DEFAULT 0,
					parent_seq  INTEGER,
					kind        TEXT NOT NULL DEFAULT '',
					title       TEXT NOT NULL DEFAULT '',
					body        TEXT,
					command     TEXT NOT NULL DEFAULT '',
					started_at  TEXT,
					ended_at    TEXT,
					created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
				);`,
				`CREATE INDEX IF NOT EXISTS idx_run_steps_run ON run_steps (run_id, seq);`,
				`CREATE INDEX IF NOT EXISTS idx_run_steps_task ON run_steps (task_id, seq);`,
			}
			for _, q := range queries {
				if _, err := conn.Exec(q); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 11,
		Name:    "task_assignee_agent",
		Up: func(conn *sql.DB) error {
			if _, err := conn.Exec("ALTER TABLE tasks ADD COLUMN assignee_agent_id TEXT;"); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
				return err
			}
			return nil
		},
	},
	{
		Version: 12,
		Name:    "run_steps_status",
		Up: func(conn *sql.DB) error {
			_, err := conn.Exec(`ALTER TABLE run_steps ADD COLUMN status TEXT NOT NULL DEFAULT '';`)
			if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
				return err
			}
			return nil
		},
	},
	{
		Version: 13,
		Name:    "task_work_kind",
		Up: func(conn *sql.DB) error {
			_, err := conn.Exec(`ALTER TABLE tasks ADD COLUMN work_kind TEXT NOT NULL DEFAULT 'coding';`)
			if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
				return err
			}
			return nil
		},
	},
	{
		Version: 14,
		Name:    "run_errors",
		Up: func(conn *sql.DB) error {
			queries := []string{
				`CREATE TABLE IF NOT EXISTS run_errors (
					id           TEXT PRIMARY KEY,
					run_id       TEXT NOT NULL,
					task_id      TEXT REFERENCES tasks(id) ON DELETE CASCADE,
					turn         INTEGER NOT NULL DEFAULT 0,
					exit_code    INTEGER NOT NULL DEFAULT 0,
					stderr_tail  TEXT NOT NULL DEFAULT '',
					duration_ms  INTEGER NOT NULL DEFAULT 0,
					model        TEXT NOT NULL DEFAULT '',
					adapter      TEXT NOT NULL DEFAULT '',
					created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
				);`,
				`CREATE INDEX IF NOT EXISTS idx_run_errors_task ON run_errors (task_id, created_at DESC);`,
				`CREATE INDEX IF NOT EXISTS idx_run_errors_run ON run_errors (run_id);`,
			}
			for _, q := range queries {
				if _, err := conn.Exec(q); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 15,
		Name:    "run_control",
		Up: func(conn *sql.DB) error {
			queries := []string{
				`CREATE TABLE IF NOT EXISTS run_control (
					task_id          TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
					pause_after_step INTEGER NOT NULL DEFAULT 0,
					stop_requested   INTEGER NOT NULL DEFAULT 0,
					updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
				);`,
				`CREATE TABLE IF NOT EXISTS run_pending_messages (
					id         INTEGER PRIMARY KEY AUTOINCREMENT,
					task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
					message    TEXT NOT NULL,
					created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
				);`,
				`CREATE INDEX IF NOT EXISTS idx_run_pending_messages_task ON run_pending_messages (task_id, id);`,
			}
			for _, q := range queries {
				if _, err := conn.Exec(q); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 16,
		Name:    "security_gate",
		Up: func(conn *sql.DB) error {
			queries := []string{
				`CREATE TABLE IF NOT EXISTS security_gate_requests (
					id           TEXT PRIMARY KEY,
					cmdline      TEXT NOT NULL,
					reasons_json TEXT NOT NULL DEFAULT '[]',
					run_id       TEXT,
					status       TEXT NOT NULL DEFAULT 'pending'
					             CHECK (status IN ('pending','approved','denied')),
					created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
					decided_at   TEXT
				);`,
				`CREATE INDEX IF NOT EXISTS idx_security_gate_pending ON security_gate_requests (status, created_at)
				 WHERE status = 'pending';`,
				`CREATE TABLE IF NOT EXISTS settings_kv (
					key        TEXT PRIMARY KEY,
					value      TEXT NOT NULL,
					updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
				);`,
			}
			for _, q := range queries {
				if _, err := conn.Exec(q); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 17,
		Name:    "ship_review",
		Up: func(conn *sql.DB) error {
			queries := []string{
				`CREATE TABLE IF NOT EXISTS ship_review_cards (
					id                TEXT PRIMARY KEY,
					task_id           TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
					branch            TEXT NOT NULL,
					head_sha          TEXT NOT NULL,
					test_steps_json   TEXT NOT NULL DEFAULT '[]',
					dev_url           TEXT NOT NULL DEFAULT '',
					dev_pid           INTEGER NOT NULL DEFAULT 0,
					status            TEXT NOT NULL DEFAULT 'pending'
					                  CHECK (status IN ('pending','approved','sent_back','rejected')),
					approved_sha      TEXT,
					main_sha          TEXT,
					send_back_comment TEXT,
					reject_comment    TEXT,
					created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
					updated_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
				);`,
				`CREATE INDEX IF NOT EXISTS idx_ship_review_task ON ship_review_cards (task_id, created_at DESC);`,
				`CREATE TABLE IF NOT EXISTS project_dev_configs (
					repo_path        TEXT PRIMARY KEY,
					dev_command      TEXT NOT NULL DEFAULT '',
					dev_url          TEXT NOT NULL DEFAULT '',
					setup_steps_json TEXT NOT NULL DEFAULT '[]',
					updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
				);`,
			}
			for _, q := range queries {
				if _, err := conn.Exec(q); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 18,
		Name:    "project_migration_config",
		Up: func(conn *sql.DB) error {
			// Add migration_globs_json and sql_editor_url to project_dev_configs.
			// ALTER TABLE … ADD COLUMN is idempotent-guarded by the duplicate-column-name check.
			cols := []struct{ name, def string }{
				{"migration_globs_json", "TEXT NOT NULL DEFAULT '[]'"},
				{"sql_editor_url", "TEXT NOT NULL DEFAULT ''"},
			}
			for _, c := range cols {
				_, err := conn.Exec(fmt.Sprintf(
					"ALTER TABLE project_dev_configs ADD COLUMN %s %s;", c.name, c.def,
				))
				if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 19,
		Name:    "ship_review_files_checks",
		Up: func(conn *sql.DB) error {
			for _, stmt := range []string{
				`ALTER TABLE ship_review_cards ADD COLUMN files_changed_json TEXT NOT NULL DEFAULT '[]';`,
				`ALTER TABLE ship_review_cards ADD COLUMN check_runs_json    TEXT NOT NULL DEFAULT '[]';`,
			} {
				if _, err := conn.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 20,
		Name:    "security_gate_audit_log",
		Up: func(conn *sql.DB) error {
			for _, stmt := range []string{
				`CREATE TABLE IF NOT EXISTS security_gate_audit_log (
					id          INTEGER PRIMARY KEY AUTOINCREMENT,
					gate_id     TEXT NOT NULL,
					actor_id    TEXT NOT NULL,
					event_type  TEXT NOT NULL,
					from_status TEXT,
					to_status   TEXT,
					payload     TEXT,
					created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
				);`,
				`CREATE INDEX IF NOT EXISTS idx_sga_gate ON security_gate_audit_log(gate_id, created_at DESC);`,
			} {
				if _, err := conn.Exec(stmt); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 21,
		Name:    "supabase_devenv",
		Up: func(conn *sql.DB) error {
			// ship_review_cards: async dev-env state and progress log.
			cardCols := []struct{ name, def string }{
				{"dev_state", "TEXT NOT NULL DEFAULT ''"},
				{"dev_log_json", "TEXT NOT NULL DEFAULT '[]'"},
			}
			for _, c := range cardCols {
				_, err := conn.Exec(fmt.Sprintf(
					"ALTER TABLE ship_review_cards ADD COLUMN %s %s;", c.name, c.def,
				))
				if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			// project_dev_configs: built-in Supabase dev env flags.
			cfgCols := []struct{ name, def string }{
				{"supabase_enabled", "INTEGER NOT NULL DEFAULT 0"},
				{"supabase_keep_up", "INTEGER NOT NULL DEFAULT 0"},
			}
			for _, c := range cfgCols {
				_, err := conn.Exec(fmt.Sprintf(
					"ALTER TABLE project_dev_configs ADD COLUMN %s %s;", c.name, c.def,
				))
				if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 22,
		Name:    "board_webauthn_credentials",
		Up: func(conn *sql.DB) error {
			_, err := conn.Exec(`CREATE TABLE IF NOT EXISTS board_webauthn_credentials (
				id             TEXT PRIMARY KEY,
				credential_id  BLOB NOT NULL,
				public_key     BLOB NOT NULL,
				sign_count     INTEGER NOT NULL DEFAULT 0,
				aaguid         TEXT NOT NULL DEFAULT '',
				created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
				updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
			);`)
			return err
		},
	},
	{
		Version: 23,
		Name:    "board_audit_log",
		Up: func(conn *sql.DB) error {
			for _, stmt := range []string{
				`CREATE TABLE IF NOT EXISTS board_audit_log (
					id         INTEGER PRIMARY KEY AUTOINCREMENT,
					actor_id   TEXT NOT NULL,
					event_type TEXT NOT NULL,
					payload    TEXT,
					created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
				);`,
				`CREATE INDEX IF NOT EXISTS idx_bal_event ON board_audit_log(event_type, created_at DESC);`,
			} {
				if _, err := conn.Exec(stmt); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 24,
		Name:    "run_steps_command",
		Up: func(conn *sql.DB) error {
			_, err := conn.Exec(`ALTER TABLE run_steps ADD COLUMN command TEXT NOT NULL DEFAULT '';`)
			if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
				return err
			}
			return nil
		},
	},
	{
		Version: 25,
		Name:    "ship_review_branch_cleanup",
		Up: func(conn *sql.DB) error {
			// STA-637: outcome of deleting the task branch after Approve & merge.
			for _, stmt := range []string{
				`ALTER TABLE ship_review_cards ADD COLUMN branch_deleted      INTEGER NOT NULL DEFAULT 0;`,
				`ALTER TABLE ship_review_cards ADD COLUMN branch_delete_error TEXT    NOT NULL DEFAULT '';`,
			} {
				if _, err := conn.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 27,
		Name:    "board_webauthn_credential_flags",
		Up: func(conn *sql.DB) error {
			// 27, not 26: PR #192 (board_alerts) claims 26 and already ran on the live DB.
			// STA-716: go-webauthn checks the stored BackupEligible flag on every
			// login, so it has to be persisted. The flag columns stay NULL on rows
			// enrolled before this migration; the first valid assertion fills them.
			for _, stmt := range []string{
				`ALTER TABLE board_webauthn_credentials ADD COLUMN flags_user_present    INTEGER;`,
				`ALTER TABLE board_webauthn_credentials ADD COLUMN flags_user_verified   INTEGER;`,
				`ALTER TABLE board_webauthn_credentials ADD COLUMN flags_backup_eligible INTEGER;`,
				`ALTER TABLE board_webauthn_credentials ADD COLUMN flags_backup_state    INTEGER;`,
				`ALTER TABLE board_webauthn_credentials ADD COLUMN attestation_type      TEXT NOT NULL DEFAULT '';`,
				`ALTER TABLE board_webauthn_credentials ADD COLUMN transports            TEXT NOT NULL DEFAULT '[]';`,
			} {
				if _, err := conn.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			return nil
		},
	},
	{
		// Board-assigned numbers: 26/27 #193, 28 this (#196), 29 #195, 30 #192.
		Version: 28,
		Name:    "project_dev_configs_live_credentials",
		Up: func(conn *sql.DB) error {
			// STA-727: previews of this project run against production credentials.
			_, err := conn.Exec(`ALTER TABLE project_dev_configs ADD COLUMN live_credentials INTEGER NOT NULL DEFAULT 0;`)
			if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
				return err
			}
			return nil
		},
	},
	{
		Version: 29,
		Name:    "ship_review_merge_modes",
		Up: func(conn *sql.DB) error {
			// STA-717: per-project merge mode (direct / open_pr / pr_merge) and
			// the GitHub PR + CI checks a PR-mode card is pinned to.
			// 29 is the Board's assignment (2026-10-05): #193 = 27,
			// #196 = 28, #195 = 29, #192 = 30.
			for _, stmt := range []string{
				`ALTER TABLE project_dev_configs ADD COLUMN merge_mode    TEXT NOT NULL DEFAULT '';`,
				`ALTER TABLE project_dev_configs ADD COLUMN gh_config_dir TEXT NOT NULL DEFAULT '';`,
				`ALTER TABLE ship_review_cards ADD COLUMN merge_mode       TEXT    NOT NULL DEFAULT '';`,
				`ALTER TABLE ship_review_cards ADD COLUMN pr_number        INTEGER NOT NULL DEFAULT 0;`,
				`ALTER TABLE ship_review_cards ADD COLUMN pr_url           TEXT    NOT NULL DEFAULT '';`,
				`ALTER TABLE ship_review_cards ADD COLUMN pr_checks_json   TEXT    NOT NULL DEFAULT '[]';`,
				`ALTER TABLE ship_review_cards ADD COLUMN pr_checks_sha    TEXT    NOT NULL DEFAULT '';`,
				`ALTER TABLE ship_review_cards ADD COLUMN pr_checks_at     TEXT    NOT NULL DEFAULT '';`,
				`ALTER TABLE ship_review_cards ADD COLUMN pr_merge_error   TEXT    NOT NULL DEFAULT '';`,
				`ALTER TABLE ship_review_cards ADD COLUMN ci_fix_requested INTEGER NOT NULL DEFAULT 0;`,
			} {
				if _, err := conn.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 31,
		Name:    "ship_review_test_gate",
		Up: func(conn *sql.DB) error {
			// 31: the Board fixed 26-30 for the open PRs (#193 26/27,
			// #196 28, #195 29, #192 30).
			// STA-734: the card's last "Test coverage" report, the project's
			// own test-exempt globs, and one "Add tests" backlog task per PR
			// merged without tests.
			for _, stmt := range []string{
				`ALTER TABLE ship_review_cards ADD COLUMN test_gate_json TEXT NOT NULL DEFAULT '';`,
				`ALTER TABLE project_dev_configs ADD COLUMN test_exempt_globs_json TEXT NOT NULL DEFAULT '[]';`,
				`CREATE TABLE IF NOT EXISTS ship_review_test_tasks (
					dedupe_key     TEXT PRIMARY KEY,
					task_id        TEXT NOT NULL DEFAULT '',
					source_task_id TEXT NOT NULL,
					card_id        TEXT NOT NULL,
					pr_number      INTEGER NOT NULL DEFAULT 0,
					head_sha       TEXT NOT NULL DEFAULT '',
					created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
				);`,
				`CREATE INDEX IF NOT EXISTS idx_ship_review_test_tasks_source ON ship_review_test_tasks (source_task_id);`,
			} {
				if _, err := conn.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 32,
		Name:    "task_worktree_bases",
		Up: func(conn *sql.DB) error {
			// STA-774: the commit each task worktree branched from, recorded
			// by the daemon. Git refs are writable by the agent in the
			// worktree; this row is what ship review and the Diff tab trust.
			_, err := conn.Exec(`CREATE TABLE IF NOT EXISTS task_worktree_bases (
				task_id    TEXT PRIMARY KEY,
				repo_path  TEXT NOT NULL DEFAULT '',
				base_sha   TEXT NOT NULL,
				created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
				updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
			);`)
			return err
		},
	},
	{
		Version: 33,
		Name:    "task_origin_and_legacy_cleanup",
		Up: func(conn *sql.DB) error {
			// Backlog stage + task origins: tasks.origin, every existing task
			// marked legacy, organization 'STA' -> 'StayPoint', and exact
			// duplicate legacy titles merged. See migrate_origin.go.
			return migrateTaskOrigins(conn)
		},
	},
	{
		// 34, not 33: 33 is left for the concurrent backlog-stage branch.
		Version: 34,
		Name:    "task_session_attachments",
		Up: func(conn *sql.DB) error {
			// STA-854: interactive agent sessions (Claude session_id, agy
			// conversationId) attached to a task with `staypoint task attach`.
			// The PreToolUse tracking gate allows writes in work repos only
			// for sessions with a row here (or a daemon run's STAYPOINT_TASK_ID).
			_, err := conn.Exec(`CREATE TABLE IF NOT EXISTS task_session_attachments (
				session_id  TEXT PRIMARY KEY,
				task_id     TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
				client      TEXT NOT NULL DEFAULT 'claude',
				repo_path   TEXT NOT NULL DEFAULT '',
				attached_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
			);`)
			return err
		},
	},
	{
		// 35: 33 is backlog-stage (task origins), 34 is STA-854 on main.
		Version: 35,
		Name:    "task_source_ref",
		Up: func(conn *sql.DB) error {
			// staypoint import paperclip: where an imported task came from.
			// source_ref is the human identifier (STA-772), source_id the
			// Paperclip uuid; the unique index makes re-imports skip.
			// The live DB already has tasks.priority (added outside the
			// ledger); fresh DBs get it here, the duplicate is ignored.
			for _, stmt := range []string{
				`ALTER TABLE tasks ADD COLUMN priority TEXT NOT NULL DEFAULT 'medium';`,
				`ALTER TABLE tasks ADD COLUMN source_ref TEXT NOT NULL DEFAULT '';`,
				`ALTER TABLE tasks ADD COLUMN source_id  TEXT NOT NULL DEFAULT '';`,
				`CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_source_id ON tasks (source_id) WHERE source_id != '';`,
			} {
				if _, err := conn.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			return nil
		},
	},
	{
		// 36: 35 is task_source_ref on main (e9d8685).
		Version: 36,
		Name:    "task_provider_choice",
		Up: func(conn *sql.DB) error {
			// Board rule GeminiRequiresExplicitChoice (STA-838): a task's
			// explicit provider ("" = default Claude, "claude", "gemini") and
			// model ("opus", "sonnet", "gemini-3.1-pro-high", ...). Gemini
			// runs only when provider is 'gemini'.
			for _, stmt := range []string{
				`ALTER TABLE tasks ADD COLUMN provider       TEXT NOT NULL DEFAULT '';`,
				`ALTER TABLE tasks ADD COLUMN model_override TEXT NOT NULL DEFAULT '';`,
			} {
				if _, err := conn.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 37,
		Name:    "gemini_code_grant_uses",
		Up: func(conn *sql.DB) error {
			// Board rule (2026-10-06): a Board-approved "Gemini code" gate
			// request (security_gate_requests.run_id = 'gemini-code') covers
			// one daemon run in a personal repo. The run that consumes it (or
			// the refusal that consumes a denial) is recorded here, so the
			// next run needs a new approval.
			_, err := conn.Exec(`CREATE TABLE IF NOT EXISTS gemini_code_grant_uses (
				gate_request_id TEXT PRIMARY KEY,
				task_id         TEXT NOT NULL DEFAULT '',
				run_id          TEXT NOT NULL DEFAULT '',
				used_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
			);`)
			return err
		},
	},
	{
		Version: 38,
		Name:    "work_product_doc_type",
		Up: func(conn *sql.DB) error {
			// STA-861: `staypoint task product add --type doc` and
			// POST /api/tasks/{id}/work-products register a doc (a design
			// doc or report link) as a work product. SQLite cannot alter a
			// CHECK constraint, so the table is rebuilt with 'doc' allowed.
			var ddl string
			if err := conn.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'task_work_products'`).Scan(&ddl); err != nil {
				return err
			}
			if strings.Contains(ddl, "'doc'") {
				return nil
			}
			tx, err := conn.Begin()
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			for _, stmt := range []string{
				`CREATE TABLE task_work_products_new (
					id            INTEGER PRIMARY KEY AUTOINCREMENT,
					task_id       TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
					product_type  TEXT NOT NULL CHECK (product_type IN ('pull_request', 'commit', 'branch', 'workspace_file', 'doc')),
					reference     TEXT NOT NULL,
					created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
				);`,
				`INSERT INTO task_work_products_new (id, task_id, product_type, reference, created_at)
					SELECT id, task_id, product_type, reference, created_at FROM task_work_products;`,
				`DROP TABLE task_work_products;`,
				`ALTER TABLE task_work_products_new RENAME TO task_work_products;`,
			} {
				if _, err := tx.Exec(stmt); err != nil {
					return err
				}
			}
			return tx.Commit()
		},
	},
	{
		// 39: 38 is work_product_doc_type (STA-861).
		Version: 39,
		Name:    "gate_rules_and_decision_log",
		Up: func(conn *sql.DB) error {
			// STA-868: gate requests carry task/repo/org context and the
			// scripts they run; Board allow rules auto-approve matching
			// requests; decision_log records advisor recommendations next to
			// the Board's decision (shared with STA-433's decision log).
			for _, stmt := range []string{
				`ALTER TABLE security_gate_requests ADD COLUMN task_id      TEXT NOT NULL DEFAULT '';`,
				`ALTER TABLE security_gate_requests ADD COLUMN repo         TEXT NOT NULL DEFAULT '';`,
				`ALTER TABLE security_gate_requests ADD COLUMN org          TEXT NOT NULL DEFAULT '';`,
				`ALTER TABLE security_gate_requests ADD COLUMN cwd          TEXT NOT NULL DEFAULT '';`,
				`ALTER TABLE security_gate_requests ADD COLUMN scripts_json TEXT NOT NULL DEFAULT '[]';`,
				`ALTER TABLE security_gate_requests ADD COLUMN decided_by   TEXT NOT NULL DEFAULT '';`,
			} {
				if _, err := conn.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			for _, stmt := range []string{
				`CREATE TABLE IF NOT EXISTS security_gate_rules (
					id             INTEGER PRIMARY KEY AUTOINCREMENT,
					pattern        TEXT NOT NULL,
					match_kind     TEXT NOT NULL DEFAULT 'exact' CHECK (match_kind IN ('exact','prefix')),
					reasons_json   TEXT NOT NULL DEFAULT '[]',
					scripts_json   TEXT NOT NULL DEFAULT '[]',
					scope          TEXT NOT NULL CHECK (scope IN ('task','repo','org')),
					scope_value    TEXT NOT NULL,
					source_gate_id TEXT NOT NULL DEFAULT '',
					note           TEXT NOT NULL DEFAULT '',
					created_by     TEXT NOT NULL DEFAULT 'board',
					created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
					expires_at     TEXT,
					deleted_at     TEXT,
					hit_count      INTEGER NOT NULL DEFAULT 0,
					last_hit_at    TEXT
				);`,
				`CREATE INDEX IF NOT EXISTS idx_sgr_scope ON security_gate_rules (scope, scope_value) WHERE deleted_at IS NULL;`,
				`CREATE TABLE IF NOT EXISTS decision_log (
					id             INTEGER PRIMARY KEY AUTOINCREMENT,
					subject_kind   TEXT NOT NULL,
					subject_id     TEXT NOT NULL,
					advisor        TEXT NOT NULL,
					model          TEXT NOT NULL DEFAULT '',
					recommendation TEXT NOT NULL DEFAULT '',
					reason         TEXT NOT NULL DEFAULT '',
					latency_ms     INTEGER NOT NULL DEFAULT 0,
					error          TEXT NOT NULL DEFAULT '',
					final_decision TEXT NOT NULL DEFAULT '',
					decided_by     TEXT NOT NULL DEFAULT '',
					decided_at     TEXT,
					created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
				);`,
				`CREATE INDEX IF NOT EXISTS idx_decision_log_subject ON decision_log (subject_kind, subject_id, advisor, id);`,
			} {
				if _, err := conn.Exec(stmt); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 40,
		Name:    "task_target_branch",
		Up: func(conn *sql.DB) error {
			// A project's work can land on a branch other than main (e.g.
			// dev-server for work repos). The Board sets it per project; each
			// task records the target it was cut from, next to its base, and
			// each card the branch its Approve merges into.
			for _, stmt := range []string{
				`ALTER TABLE project_dev_configs ADD COLUMN target_branch TEXT NOT NULL DEFAULT '';`,
				`ALTER TABLE task_worktree_bases ADD COLUMN target_branch TEXT NOT NULL DEFAULT '';`,
				`ALTER TABLE ship_review_cards   ADD COLUMN target_branch TEXT NOT NULL DEFAULT '';`,
			} {
				if _, err := conn.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			return nil
		},
	},
	{
		Version: 41,
		Name:    "gate_task_trust",
		Up: func(conn *sql.DB) error {
			// task-6c1ed91f: "Trust this task until…" is a task-scoped allow
			// rule with match_kind 'any' and an optional tev1 mode. SQLite
			// cannot alter a CHECK, so the rules table is rebuilt. Requests
			// gain defer_at (when an under-trust delete outside the worktree
			// stops waiting) and deferred_at (when it was skipped).
			for _, stmt := range []string{
				`ALTER TABLE security_gate_requests ADD COLUMN defer_at    TEXT;`,
				`ALTER TABLE security_gate_requests ADD COLUMN deferred_at TEXT;`,
			} {
				if _, err := conn.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			tx, err := conn.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback()
			for _, stmt := range []string{
				`CREATE TABLE security_gate_rules_new (
					id             INTEGER PRIMARY KEY AUTOINCREMENT,
					pattern        TEXT NOT NULL,
					match_kind     TEXT NOT NULL DEFAULT 'exact' CHECK (match_kind IN ('exact','prefix','any')),
					reasons_json   TEXT NOT NULL DEFAULT '[]',
					scripts_json   TEXT NOT NULL DEFAULT '[]',
					scope          TEXT NOT NULL CHECK (scope IN ('task','repo','org')),
					scope_value    TEXT NOT NULL,
					source_gate_id TEXT NOT NULL DEFAULT '',
					note           TEXT NOT NULL DEFAULT '',
					created_by     TEXT NOT NULL DEFAULT 'board',
					created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
					expires_at     TEXT,
					deleted_at     TEXT,
					hit_count      INTEGER NOT NULL DEFAULT 0,
					last_hit_at    TEXT,
					tev1           INTEGER NOT NULL DEFAULT 0,
					tev1_threshold REAL NOT NULL DEFAULT 0,
					ended_reason   TEXT NOT NULL DEFAULT '',
					CHECK (match_kind <> 'any' OR (scope = 'task' AND expires_at IS NOT NULL))
				);`,
				`INSERT INTO security_gate_rules_new
					(id, pattern, match_kind, reasons_json, scripts_json, scope, scope_value, source_gate_id, note,
					 created_by, created_at, expires_at, deleted_at, hit_count, last_hit_at)
				 SELECT id, pattern, match_kind, reasons_json, scripts_json, scope, scope_value, source_gate_id, note,
					 created_by, created_at, expires_at, deleted_at, hit_count, last_hit_at
				 FROM security_gate_rules;`,
				`DROP TABLE security_gate_rules;`,
				`ALTER TABLE security_gate_rules_new RENAME TO security_gate_rules;`,
				`CREATE INDEX IF NOT EXISTS idx_sgr_scope ON security_gate_rules (scope, scope_value) WHERE deleted_at IS NULL;`,
				`CREATE INDEX IF NOT EXISTS idx_sgreq_task ON security_gate_requests (task_id, created_at);`,
			} {
				if _, err := tx.Exec(stmt); err != nil {
					return err
				}
			}
			return tx.Commit()
		},
	},
	{
		Version: 42,
		Name:    "decision_log_shadow_columns",
		Up: func(conn *sql.DB) error {
			// STA-433.1: shadow decisions (docs/rfc/003) log the current
			// and local pick for any decision type in decision_log. Gate
			// advice rows read '' in the new columns.
			for _, stmt := range []string{
				`ALTER TABLE decision_log ADD COLUMN decision_key TEXT NOT NULL DEFAULT '';`,
				`ALTER TABLE decision_log ADD COLUMN task_id      TEXT NOT NULL DEFAULT '';`,
				`ALTER TABLE decision_log ADD COLUMN question     TEXT NOT NULL DEFAULT '';`,
				`ALTER TABLE decision_log ADD COLUMN options_json TEXT NOT NULL DEFAULT '';`,
				`ALTER TABLE decision_log ADD COLUMN pick         TEXT NOT NULL DEFAULT '';`,
			} {
				if _, err := conn.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
					return err
				}
			}
			_, err := conn.Exec(`CREATE INDEX IF NOT EXISTS idx_decision_log_kind_created ON decision_log (subject_kind, created_at);`)
			return err
		},
	},
	{
		// 43: task-d4145d27 / #245-2. (42 is PR #247's decision_log
		// shadow columns.)
		Version: 43,
		Name:    "work_product_provenance",
		Up: func(conn *sql.DB) error {
			// #245-2: who created a registered branch. Approve deletes only
			// a branch StayPoint made (staypoint) or the task created first
			// (task); existing rows stay '' (unknown) and are never deleted.
			_, err := conn.Exec(`ALTER TABLE task_work_products ADD COLUMN provenance TEXT NOT NULL DEFAULT '';`)
			if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
				return err
			}
			return nil
		},
	},
}

func copyFile(src, dst string) error {
	input, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, input, 0644)
}

// validateMigrations rejects a migration list that two PRs both numbered
// the same way. Run at startup and in CI (TestMigrations_VersionsUnique).
func validateMigrations(ms []Migration) error {
	seen := make(map[int]string, len(ms))
	for _, m := range ms {
		if m.Version <= 0 {
			return fmt.Errorf("migration %q has invalid version %d", m.Name, m.Version)
		}
		if prev, dup := seen[m.Version]; dup {
			return fmt.Errorf("duplicate migration version %d: %q and %q; renumber one of them", m.Version, prev, m.Name)
		}
		seen[m.Version] = m.Name
	}
	return nil
}

func applyMigrations(dbPath string, conn *sql.DB) error {
	return applyMigrationSet(dbPath, conn, Migrations)
}

// applyMigrationSet runs every migration not recorded in schema_migrations, in
// version order. Parallel PRs can merge a lower version after a higher one has
// already run (STA-744); comparing against MAX(version) skipped those silently.
func applyMigrationSet(dbPath string, conn *sql.DB, ms []Migration) error {
	if err := validateMigrations(ms); err != nil {
		return err
	}

	// schema_versions is the pre-STA-744 ledger. It is still written so an
	// older binary, which reads MAX(version), sees the DB as current.
	if _, err := conn.Exec(`
		CREATE TABLE IF NOT EXISTS schema_versions (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		);
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL DEFAULT '',
			applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		);
	`); err != nil {
		return fmt.Errorf("failed to create migration tables: %w", err)
	}

	if err := backfillSchemaMigrations(conn, ms); err != nil {
		return err
	}

	applied := map[int]bool{}
	rows, err := conn.Query(`SELECT version FROM schema_migrations;`)
	if err != nil {
		return fmt.Errorf("failed to read applied migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("failed to read applied migrations: %w", err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read applied migrations: %w", err)
	}

	var pending []Migration
	for _, m := range ms {
		if !applied[m.Version] {
			pending = append(pending, m)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Version < pending[j].Version })

	if len(pending) == 0 {
		return nil
	}

	// Backup before migrating
	if _, err := conn.Exec("PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
		return fmt.Errorf("failed to checkpoint wal before backup: %w", err)
	}

	backupPath := dbPath + ".bak"
	if err := copyFile(dbPath, backupPath); err != nil {
		return fmt.Errorf("failed to backup database: %w", err)
	}

	for _, m := range pending {
		if err := m.Up(conn); err != nil {
			// Restore backup
			conn.Close()
			_ = copyFile(backupPath, dbPath)
			_ = os.Remove(dbPath + "-wal")
			_ = os.Remove(dbPath + "-shm")
			return fmt.Errorf("migration %d (%s) failed, database restored from backup: %w", m.Version, m.Name, err)
		}
		if err := recordMigration(conn, m); err != nil {
			conn.Close()
			_ = copyFile(backupPath, dbPath)
			_ = os.Remove(dbPath + "-wal")
			_ = os.Remove(dbPath + "-shm")
			return fmt.Errorf("failed to record migration %d: %w", m.Version, err)
		}
	}

	_ = os.Remove(backupPath)
	return nil
}

func recordMigration(conn *sql.DB, m Migration) error {
	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`, m.Version, m.Name); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO schema_versions (version) VALUES (?)`, m.Version); err != nil {
		return err
	}
	return tx.Commit()
}

// backfillSchemaMigrations copies every version recorded in schema_versions
// into schema_migrations. It copies the recorded set rather than assuming
// 1..MAX: a DB migrated by sibling branch builds can hold 1..26,28 with 27
// never run, and 27 must stay pending. Runs on every open, so a version an
// older binary recorded after a downgrade is not run twice. OR IGNORE: two
// processes opening the same pre-ledger DB both see the rows missing.
func backfillSchemaMigrations(conn *sql.DB, ms []Migration) error {
	names := make(map[int]string, len(ms))
	for _, m := range ms {
		names[m.Version] = m.Name
	}

	type legacyRow struct {
		version   int
		appliedAt string
	}
	rows, err := conn.Query(`
		SELECT version, applied_at FROM schema_versions
		WHERE version NOT IN (SELECT version FROM schema_migrations)
		ORDER BY version;
	`)
	if err != nil {
		return fmt.Errorf("failed to read schema_versions for backfill: %w", err)
	}
	var missing []legacyRow
	for rows.Next() {
		var r legacyRow
		if err := rows.Scan(&r.version, &r.appliedAt); err != nil {
			rows.Close()
			return fmt.Errorf("failed to read schema_versions for backfill: %w", err)
		}
		missing = append(missing, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read schema_versions for backfill: %w", err)
	}
	if len(missing) == 0 {
		return nil
	}

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("failed to backfill schema_migrations: %w", err)
	}
	defer tx.Rollback()
	for _, r := range missing {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			r.version, names[r.version], r.appliedAt); err != nil {
			return fmt.Errorf("failed to backfill schema_migrations version %d: %w", r.version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to backfill schema_migrations: %w", err)
	}
	return nil
}

func Open(dbPath string) (*Store, error) {
	if err := refuseLiveDBUnderTest(dbPath); err != nil {
		return nil, err
	}
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create db dir: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)", dbPath)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	conn.SetMaxOpenConns(1)

	// applyMigrations reads the ledger only after the lock is held, so an
	// opener that waited sees the winner's migrations as applied and runs none.
	unlock, err := lockMigrations(dbPath)
	if err != nil {
		conn.Close()
		return nil, err
	}
	err = applyMigrations(dbPath, conn)
	if err == nil {
		// A ledger row says a migration ran, but a branch build can record a
		// version whose DDL differed from main's: recreate missing tables.
		_, err = RepairSchema(conn, nil)
	}
	unlock()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to migrate schema: %w", err)
	}

	// Always execute pragmas on connect just in case
	if _, err := conn.Exec(`
		PRAGMA journal_mode = WAL;
		PRAGMA synchronous = NORMAL;
		PRAGMA busy_timeout = 5000;
		PRAGMA foreign_keys = ON;
	`); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to apply pragmas: %w", err)
	}

	return &Store{db: conn}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) DB() *sql.DB {
	return s.db
}
