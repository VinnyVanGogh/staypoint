package db

import (
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
)

// Schema self-check (STA-868).
//
// schema_versions says which migrations ran, not what they created: a branch
// build can record version N with different DDL than main's migration N, so
// main's tables for N never exist (the live DB had migration 20 recorded but
// no security_gate_audit_log, so every gate decision's audit write failed).
//
// RepairSchema replays every migration on an empty in-memory database to learn
// which tables each one creates and their final DDL, then creates any table
// (and its indexes) that the live DB lacks although its migration is recorded.
// It only issues CREATE TABLE / CREATE INDEX IF NOT EXISTS; it never alters,
// drops or writes rows.

// RepairAction describes one repaired object.
type RepairAction struct {
	Table     string
	Migration int
}

type refObject struct {
	typ, name, table, sql string
	version               int
}

var createPrefixRe = regexp.MustCompile(`(?i)^\s*CREATE\s+(UNIQUE\s+INDEX|INDEX|VIRTUAL\s+TABLE|TABLE)\s+(IF\s+NOT\s+EXISTS\s+)?`)

// referenceSchema replays Migrations on an in-memory DB and returns the final
// tables and indexes, each tagged with the migration that first created it.
func referenceSchema(migs []Migration) ([]refObject, error) {
	mem, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	defer mem.Close()
	mem.SetMaxOpenConns(1)
	firstSeen := map[string]int{}
	snapshot := func(version int) error {
		rows, err := mem.Query(`SELECT type || ':' || name FROM sqlite_master WHERE type IN ('table','index')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				return err
			}
			if _, ok := firstSeen[k]; !ok {
				firstSeen[k] = version
			}
		}
		return rows.Err()
	}
	sorted := append([]Migration(nil), migs...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].Version < sorted[j-1].Version; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	for _, m := range sorted {
		if err := m.Up(mem); err != nil {
			return nil, fmt.Errorf("replay migration %d: %w", m.Version, err)
		}
		if err := snapshot(m.Version); err != nil {
			return nil, err
		}
	}
	rows, err := mem.Query(`SELECT type, name, tbl_name, sql FROM sqlite_master
		WHERE type IN ('table','index') AND sql IS NOT NULL AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []refObject
	for rows.Next() {
		var o refObject
		if err := rows.Scan(&o.typ, &o.name, &o.table, &o.sql); err != nil {
			return nil, err
		}
		o.version = firstSeen[o.typ+":"+o.name]
		out = append(out, o)
	}
	return out, rows.Err()
}

// ifNotExists rewrites a sqlite_master CREATE statement to be idempotent.
func ifNotExists(stmt string) string {
	loc := createPrefixRe.FindStringSubmatchIndex(stmt)
	if loc == nil {
		return ""
	}
	kind := stmt[loc[2]:loc[3]]
	return "CREATE " + kind + " IF NOT EXISTS " + stmt[loc[1]:]
}

// builtinMigrations is the compiled-in migration list, captured before tests
// can append to Migrations; only these are replayed for the reference schema,
// once per process.
var (
	builtinMigrations = Migrations[:len(Migrations):len(Migrations)]
	refOnce           sync.Once
	refCached         []refObject
	refErr            error
)

func builtinReference() ([]refObject, error) {
	refOnce.Do(func() { refCached, refErr = referenceSchema(builtinMigrations) })
	return refCached, refErr
}

// RepairSchema creates tables missing from conn whose creating migration is
// recorded as applied. log receives one line per repair (nil = slog).
func RepairSchema(conn *sql.DB, log func(string)) ([]RepairAction, error) {
	return repairSchema(conn, builtinReference, log)
}

func repairSchema(conn *sql.DB, reference func() ([]refObject, error), log func(string)) ([]RepairAction, error) {
	if log == nil {
		log = func(s string) { slog.Warn(s) }
	}
	applied := map[int]bool{}
	rows, err := conn.Query(`SELECT version FROM schema_versions`)
	if err != nil {
		return nil, fmt.Errorf("schema repair: read ledger: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, err
		}
		applied[v] = true
	}
	rows.Close()

	live := map[string]bool{}
	rows, err = conn.Query(`SELECT type || ':' || name FROM sqlite_master`)
	if err != nil {
		return nil, fmt.Errorf("schema repair: read schema: %w", err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return nil, err
		}
		live[k] = true
	}
	rows.Close()

	ref, err := reference()
	if err != nil {
		// Never block startup on the self-check itself.
		log("schema repair: skipped: " + err.Error())
		return nil, nil
	}
	var virtual []string
	for _, o := range ref {
		if o.typ == "table" && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(o.sql)), "CREATE VIRTUAL") {
			virtual = append(virtual, o.name)
		}
	}
	shadow := func(name string) bool {
		for _, v := range virtual {
			if name != v && strings.HasPrefix(name, v+"_") {
				return true
			}
		}
		return false
	}

	var actions []RepairAction
	repaired := map[string]bool{}
	for _, o := range ref {
		if o.typ != "table" || live["table:"+o.name] || !applied[o.version] || shadow(o.name) {
			continue
		}
		stmt := ifNotExists(o.sql)
		if stmt == "" {
			continue
		}
		if _, err := conn.Exec(stmt); err != nil {
			return actions, fmt.Errorf("schema repair: create %s: %w", o.name, err)
		}
		repaired[o.name] = true
		actions = append(actions, RepairAction{Table: o.name, Migration: o.version})
		log(fmt.Sprintf("schema repair: created %s (migration %d recorded but table missing)", o.name, o.version))
	}
	for _, o := range ref {
		if o.typ != "index" || !repaired[o.table] || live["index:"+o.name] {
			continue
		}
		if stmt := ifNotExists(o.sql); stmt != "" {
			if _, err := conn.Exec(stmt); err != nil {
				return actions, fmt.Errorf("schema repair: create index %s: %w", o.name, err)
			}
		}
	}
	return actions, nil
}
