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
}

func copyFile(src, dst string) error {
	input, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, input, 0644)
}

func applyMigrations(dbPath string, conn *sql.DB) error {
	if _, err := conn.Exec(`
		CREATE TABLE IF NOT EXISTS schema_versions (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		);
	`); err != nil {
		return fmt.Errorf("failed to create schema_versions: %w", err)
	}

	// Apply every migration not yet recorded, not just those above MAX(version):
	// branch builds of parallel PRs migrate the live DB, so it can already hold
	// a higher version while a lower one from another branch never ran (STA-716).
	rows, err := conn.Query(`SELECT version FROM schema_versions;`)
	if err != nil {
		return fmt.Errorf("failed to read applied versions: %w", err)
	}
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("failed to read applied versions: %w", err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read applied versions: %w", err)
	}

	var pending []Migration
	for _, m := range Migrations {
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
		if _, err := conn.Exec(`INSERT INTO schema_versions (version) VALUES (?)`, m.Version); err != nil {
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

func Open(dbPath string) (*Store, error) {
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
