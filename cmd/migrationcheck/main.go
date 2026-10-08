// Command migrationcheck fails when the migration list in internal/db/db.go
// reuses a version number: twice in the working tree, or once more on top of
// a version the base branch already ships under another name.
//
// #247 numbered its migration 40 while main already had 40
// (task_target_branch). A DB that had run main's 40 would record the version
// as applied and never run the PR's migration. TestMigrations_VersionsUnique
// only sees one tree, so it misses the case where the PR replaced main's
// entry or was tested before main took the number.
//
// Usage (CI runs it against a freshly fetched origin/main):
//
//	go run ./cmd/migrationcheck -base origin/main
package main

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

const defaultFile = "internal/db/db.go"

type migration struct {
	Version int
	Name    string
}

func main() {
	base := flag.String("base", "origin/main", "git ref to compare against")
	file := flag.String("file", defaultFile, "repo-relative file holding var Migrations")
	head := flag.String("head", "", "git ref to read instead of the working tree")
	flag.Parse()

	if err := run(*base, *head, *file); err != nil {
		fmt.Fprintln(os.Stderr, "migrationcheck:", err)
		os.Exit(1)
	}
}

func run(baseRef, headRef, file string) error {
	headSrc, err := readSource(headRef, file)
	if err != nil {
		return err
	}
	baseSrc, err := readSource(baseRef, file)
	if err != nil {
		return err
	}
	headMs, err := parseMigrations(headSrc)
	if err != nil {
		return fmt.Errorf("head: %w", err)
	}
	baseMs, err := parseMigrations(baseSrc)
	if err != nil {
		return fmt.Errorf("base %s: %w", baseRef, err)
	}
	if problems := check(baseMs, headMs); len(problems) > 0 {
		return fmt.Errorf("migration versions collide with %s:\n  %s", baseRef, strings.Join(problems, "\n  "))
	}
	fmt.Printf("migrationcheck: %d migrations, no version collisions with %s\n", len(headMs), baseRef)
	return nil
}

// readSource reads file from the working tree when ref is empty, else from
// the git object database.
func readSource(ref, file string) ([]byte, error) {
	if ref == "" {
		return os.ReadFile(file)
	}
	out, err := exec.Command("git", "show", ref+":"+file).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("git show %s:%s: %v: %s", ref, file, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("git show %s:%s: %w", ref, file, err)
	}
	return out, nil
}

// parseMigrations returns the entries of the package-level `var Migrations`
// composite literal. Version must be an integer literal and Name a string
// literal, so the check can't be dodged by a computed number.
func parseMigrations(src []byte) ([]migration, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, 0)
	if err != nil {
		return nil, err
	}
	var lit *ast.CompositeLit
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if n.Name == "Migrations" && i < len(vs.Values) {
					lit, _ = vs.Values[i].(*ast.CompositeLit)
				}
			}
		}
	}
	if lit == nil {
		return nil, errors.New("no `var Migrations = []Migration{...}` literal found")
	}

	ms := make([]migration, 0, len(lit.Elts))
	for i, e := range lit.Elts {
		el, ok := e.(*ast.CompositeLit)
		if !ok {
			return nil, fmt.Errorf("Migrations[%d] is not a composite literal", i)
		}
		m := migration{Version: -1}
		for _, kv := range el.Elts {
			kve, ok := kv.(*ast.KeyValueExpr)
			if !ok {
				return nil, fmt.Errorf("Migrations[%d] must use keyed fields", i)
			}
			key, _ := kve.Key.(*ast.Ident)
			if key == nil {
				continue
			}
			bl, _ := kve.Value.(*ast.BasicLit)
			switch key.Name {
			case "Version":
				if bl == nil || bl.Kind != token.INT {
					return nil, fmt.Errorf("Migrations[%d].Version must be an integer literal", i)
				}
				v, err := strconv.Atoi(bl.Value)
				if err != nil {
					return nil, fmt.Errorf("Migrations[%d].Version: %w", i, err)
				}
				m.Version = v
			case "Name":
				if bl == nil || bl.Kind != token.STRING {
					return nil, fmt.Errorf("Migrations[%d].Name must be a string literal", i)
				}
				s, err := strconv.Unquote(bl.Value)
				if err != nil {
					return nil, fmt.Errorf("Migrations[%d].Name: %w", i, err)
				}
				m.Name = s
			}
		}
		if m.Version < 0 {
			return nil, fmt.Errorf("Migrations[%d] has no Version", i)
		}
		ms = append(ms, m)
	}
	return ms, nil
}

// check lists every version used twice in head, and every head version that
// base already ships under a different name.
func check(base, head []migration) []string {
	var problems []string

	seen := map[int]string{}
	for _, m := range head {
		if prev, dup := seen[m.Version]; dup {
			problems = append(problems, fmt.Sprintf("version %d used twice: %q and %q", m.Version, prev, m.Name))
			continue
		}
		seen[m.Version] = m.Name
	}

	onBase := map[int]string{}
	maxBase := 0
	for _, m := range base {
		onBase[m.Version] = m.Name
		if m.Version > maxBase {
			maxBase = m.Version
		}
	}
	var clashes []int
	for v, name := range seen {
		if baseName, ok := onBase[v]; ok && baseName != name {
			clashes = append(clashes, v)
		}
	}
	sort.Ints(clashes)
	for _, v := range clashes {
		problems = append(problems, fmt.Sprintf("version %d is %q here but %q on base; renumber to %d or later", v, seen[v], onBase[v], maxBase+1))
	}
	return problems
}
