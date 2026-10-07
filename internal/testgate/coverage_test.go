package testgate

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestWorkflowRunsTests(t *testing.T) {
	yes := map[string]string{
		"go test":    "jobs:\n  t:\n    steps:\n      - run: go test ./...\n",
		"gotestsum":  "      - run: gotestsum -- ./...\n",
		"pytest":     "      - run: |\n          pip install -e .\n          pytest -q\n",
		"py -m":      "      - run: python -m pytest tests\n",
		"unittest":   "      - run: python3 -m unittest discover\n",
		"npm test":   "      - run: npm test\n",
		"npm run":    "      - run: npm run test:unit\n",
		"pnpm":       "      - run: pnpm test\n",
		"yarn":       "      - run: yarn test --ci\n",
		"vitest":     "      - run: npx vitest run\n",
		"playwright": "      - run: npx playwright test\n",
		"cargo":      "      - run: cargo test --all\n",
		"make":       "      - run: make test\n",
		"tox":        "      - run: tox -e py312\n",
		"dotnet":     "      - run: dotnet test\n",
	}
	for name, y := range yes {
		if !WorkflowRunsTests(y) {
			t.Errorf("%s: WorkflowRunsTests = false, want true for\n%s", name, y)
		}
	}
	no := map[string]string{
		"lint only":     "      - run: golangci-lint run\n      - run: go vet ./...\n",
		"build only":    "      - run: go build ./...\n      - run: npm run build\n",
		"commented out": "      # - run: go test ./...\n      - run: echo hi\n",
		"release":       "      - uses: goreleaser/goreleaser-action@v5\n",
		"word inside":   "      - run: echo latest testing contest\n",
	}
	for name, y := range no {
		if WorkflowRunsTests(y) {
			t.Errorf("%s: WorkflowRunsTests = true, want false for\n%s", name, y)
		}
	}
}

func TestParseChangedLines(t *testing.T) {
	diff := `diff --git a/internal/a.go b/internal/a.go
index 111..222 100644
--- a/internal/a.go
+++ b/internal/a.go
@@ -10,0 +11,3 @@ func A() {
+	x := 1
+	y := 2
+	_ = x + y
@@ -40 +43 @@ func B() {
-	old()
+	new()
@@ -50,2 +53,0 @@ func C() {
-	gone()
-	gone()
diff --git a/web/new.ts b/web/new.ts
new file mode 100644
--- /dev/null
+++ b/web/new.ts
@@ -0,0 +1,2 @@
+export const a = 1;
+export const b = 2;
diff --git a/old.py b/old.py
deleted file mode 100644
--- a/old.py
+++ /dev/null
@@ -1,2 +0,0 @@
-x = 1
-y = 2
`
	got := ParseChangedLines(diff)
	want := map[string][]int{
		"internal/a.go": {11, 12, 13, 43},
		"web/new.ts":    {1, 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseChangedLines = %v, want %v", got, want)
	}
}

func TestFormatLines(t *testing.T) {
	if got := FormatLines([]int{12, 10, 11, 40, 42, 43, 44}); got != "10-12, 40, 42-44" {
		t.Errorf("FormatLines = %q", got)
	}
	if got := FormatLines(nil); got != "" {
		t.Errorf("FormatLines(nil) = %q", got)
	}
}

const goProfile = `mode: set
github.com/acme/app/internal/calc/calc.go:5.24,7.2 1 1
github.com/acme/app/internal/calc/calc.go:9.24,10.12 1 0
github.com/acme/app/internal/calc/calc.go:10.12,12.3 1 0
github.com/acme/app/internal/calc/calc.go:13.2,13.10 1 0
github.com/acme/app/internal/calc/calc.go:9.24,10.12 1 0
`

func TestParseGoCoverProfile(t *testing.T) {
	cov, err := ParseCoverage("coverage.out", []byte(goProfile))
	if err != nil {
		t.Fatal(err)
	}
	if cov.Format != FormatGo {
		t.Fatalf("Format = %q", cov.Format)
	}
	fc := cov.Lookup("internal/calc/calc.go")
	if fc == nil {
		t.Fatal("Lookup by repo-relative suffix failed")
	}
	if fc.Lines[6] != 1 || fc.Lines[10] != 0 || fc.Lines[11] != 0 || fc.Lines[13] != 0 {
		t.Errorf("Lines = %v", fc.Lines)
	}
	if _, ok := fc.Lines[8]; ok {
		t.Errorf("line 8 is not instrumented, got %v", fc.Lines)
	}
}

// Two test binaries covering the same block: a hit in either wins.
func TestParseGoCoverProfileTakesMaxHits(t *testing.T) {
	p := "mode: count\nm/x.go:3.1,4.2 1 0\nm/x.go:3.1,4.2 1 5\n"
	cov, err := ParseCoverage("c.out", []byte(p))
	if err != nil {
		t.Fatal(err)
	}
	if got := cov.Lookup("x.go").Lines[3]; got != 5 {
		t.Errorf("hits = %d, want 5", got)
	}
}

const cobertura = `<?xml version="1.0" ?>
<coverage line-rate="0.5" version="7.4">
  <sources><source>/home/runner/work/app/app/src</source></sources>
  <packages><package name="billing">
    <classes><class name="invoice.py" filename="billing/invoice.py" line-rate="0.5">
      <methods>
        <method name="total" signature="" line-rate="1"><lines><line number="3" hits="2"/><line number="4" hits="2"/></lines></method>
        <method name="refund" signature="" line-rate="0"><lines><line number="7" hits="0"/><line number="8" hits="0"/></lines></method>
      </methods>
      <lines>
        <line number="3" hits="2"/><line number="4" hits="2"/>
        <line number="7" hits="0"/><line number="8" hits="0"/>
      </lines>
    </class></classes>
  </package></packages>
</coverage>`

func TestParseCobertura(t *testing.T) {
	cov, err := ParseCoverage("coverage.xml", []byte(cobertura))
	if err != nil {
		t.Fatal(err)
	}
	if cov.Format != FormatCobertura {
		t.Fatalf("Format = %q", cov.Format)
	}
	// The report path is relative to <source>; the repo path has the src/ prefix.
	fc := cov.Lookup("src/billing/invoice.py")
	if fc == nil {
		t.Fatal("Lookup src/billing/invoice.py failed")
	}
	if fc.Lines[3] != 2 || fc.Lines[7] != 0 {
		t.Errorf("Lines = %v", fc.Lines)
	}
	want := []FuncCoverage{{Name: "total", Start: 3, End: 4, Hits: 2}, {Name: "refund", Start: 7, End: 8, Hits: 0}}
	if !reflect.DeepEqual(fc.Funcs, want) {
		t.Errorf("Funcs = %+v, want %+v", fc.Funcs, want)
	}
}

const lcov = `TN:
SF:/home/runner/work/app/app/web/src/cart.ts
FN:1,addItem
FN:6,removeItem
FNDA:3,addItem
FNDA:0,removeItem
DA:1,3
DA:2,3
DA:6,0
DA:7,0
DA:8,0
end_of_record
`

func TestParseLCOV(t *testing.T) {
	cov, err := ParseCoverage("lcov.info", []byte(lcov))
	if err != nil {
		t.Fatal(err)
	}
	if cov.Format != FormatLCOV {
		t.Fatalf("Format = %q", cov.Format)
	}
	fc := cov.Lookup("web/src/cart.ts")
	if fc == nil {
		t.Fatal("Lookup failed")
	}
	if fc.Lines[2] != 3 || fc.Lines[7] != 0 {
		t.Errorf("Lines = %v", fc.Lines)
	}
	// FN gives only a start line; the end is the line before the next
	// function, or the last instrumented line.
	want := []FuncCoverage{{Name: "addItem", Start: 1, End: 5, Hits: 3}, {Name: "removeItem", Start: 6, End: 8, Hits: 0}}
	if !reflect.DeepEqual(fc.Funcs, want) {
		t.Errorf("Funcs = %+v, want %+v", fc.Funcs, want)
	}
}

func TestParseCoverageRejectsUnknown(t *testing.T) {
	if _, err := ParseCoverage("notes.txt", []byte("hello")); err == nil {
		t.Error("want error for a non-coverage file")
	}
}

func TestLookupPrefersLongestMatch(t *testing.T) {
	cov := &Coverage{Format: FormatGo, Files: map[string]*FileCoverage{
		"m/a/util.go":   {Lines: map[int]int{1: 1}},
		"m/b/a/util.go": {Lines: map[int]int{1: 0}},
	}}
	if got := cov.Lookup("b/a/util.go"); got == nil || got.Lines[1] != 0 {
		t.Errorf("Lookup(b/a/util.go) picked the wrong file: %+v", got)
	}
	if got := cov.Lookup("util.go"); got != nil {
		t.Errorf("ambiguous base-name lookup should miss, got %+v", got)
	}
}

const calcGo = `package calc

import "errors"

func Add(a, b int) int {
	return a + b
}

func Div(a, b int) (int, error) {
	if b == 0 {
		return 0, errors.New("div by zero")
	}
	return a / b, nil
}

type T struct{}

func (t *T) M() {}
`

func TestGoFuncRanges(t *testing.T) {
	got := GoFuncRanges([]byte(calcGo))
	want := []FuncCoverage{{Name: "Add", Start: 5, End: 7}, {Name: "Div", Start: 9, End: 14}, {Name: "(*T).M", Start: 18, End: 18}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GoFuncRanges = %+v, want %+v", got, want)
	}
}

func TestFindUncoveredGo(t *testing.T) {
	cov, err := ParseCoverage("coverage.out", []byte(goProfile))
	if err != nil {
		t.Fatal(err)
	}
	changed := map[string][]int{
		"internal/calc/calc.go": {6, 10, 11, 13},
		"internal/other/x.go":   {3},  // Go file the report doesn't list
		"web/app.js":            {12}, // not a language this report covers
	}
	src := func(p string) []byte {
		if p == "internal/calc/calc.go" {
			return []byte(calcGo)
		}
		return nil
	}
	got := FindUncovered(changed, cov, src)
	want := []UncoveredFile{
		{Path: "internal/calc/calc.go", Lines: []int{10, 11, 13}, Functions: []string{"Div"}},
		{Path: "internal/other/x.go", NotInReport: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindUncovered =\n%+v\nwant\n%+v", got, want)
	}
}

func TestFindUncoveredLCOVAndFullyCovered(t *testing.T) {
	cov, _ := ParseCoverage("lcov.info", []byte(lcov))
	got := FindUncovered(map[string][]int{"web/src/cart.ts": {2, 7}}, cov, nil)
	want := []UncoveredFile{{Path: "web/src/cart.ts", Lines: []int{7}, Functions: []string{"removeItem"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got := FindUncovered(map[string][]int{"web/src/cart.ts": {1, 2}}, cov, nil); len(got) != 0 {
		t.Errorf("fully covered change should report nothing, got %+v", got)
	}
}

func TestMergeCoverage(t *testing.T) {
	a, _ := ParseCoverage("c.out", []byte("mode: set\nm/x.go:3.1,3.9 1 0\n"))
	b, _ := ParseCoverage("c2.out", []byte("mode: set\nm/x.go:3.1,3.9 1 1\nm/y.go:1.1,1.9 1 0\n"))
	m := MergeCoverage(a, b)
	if m.Lookup("x.go").Lines[3] != 1 || m.Lookup("y.go") == nil {
		t.Errorf("merge lost data: %+v", m.Files)
	}
	if !strings.Contains(m.Format, FormatGo) {
		t.Errorf("Format = %q", m.Format)
	}
}

// STA-766 review: a CI matrix uploads one LCOV per runner, so the same file
// shows up under two absolute paths. That is one file, not an ambiguity.
func TestLookupMergesSameFileFromMatrixReports(t *testing.T) {
	ubuntu, _ := ParseCoverage("lcov.info", []byte("SF:/home/runner/work/app/app/src/util.ts\nFN:1,f\nFNDA:0,f\nDA:1,1\nDA:2,0\nend_of_record\n"))
	macos, _ := ParseCoverage("lcov.info", []byte("SF:/Users/runner/work/app/app/src/util.ts\nFN:1,f\nFNDA:3,f\nDA:1,0\nDA:2,1\nend_of_record\n"))
	cov := MergeCoverage(ubuntu, macos)
	if got := FindUncovered(map[string][]int{"src/util.ts": {1, 2}}, cov, nil); len(got) != 0 {
		t.Fatalf("a file covered across matrix jobs must not be uncovered, got %+v", got)
	}
	fc, ambiguous := cov.lookup("src/util.ts")
	if ambiguous || fc == nil || fc.Lines[1] != 1 || fc.Lines[2] != 1 {
		t.Fatalf("lookup = %+v ambiguous=%v, want merged lines hit", fc, ambiguous)
	}
	if len(fc.Funcs) != 1 || fc.Funcs[0].Hits != 3 {
		t.Errorf("Funcs = %+v, want one f with the max hits", fc.Funcs)
	}
}

// Two files in one report sharing a suffix (monorepo src/index.ts and
// packages/web/src/index.ts) are different files: coverage is unknown, which
// FindUncovered reports as Ambiguous, never as "not in the report".
func TestFindUncoveredAmbiguousIsNotNotInReport(t *testing.T) {
	one, _ := ParseCoverage("lcov.info", []byte("SF:/w/app/src/index.ts\nDA:1,1\nend_of_record\nSF:/w/app/packages/web/src/index.ts\nDA:1,0\nend_of_record\n"))
	other, _ := ParseCoverage("lcov.info", []byte("SF:/x/app/src/index.ts\nDA:1,1\nend_of_record\n"))
	for name, cov := range map[string]*Coverage{"single": one, "merged": MergeCoverage(one, other)} {
		got := FindUncovered(map[string][]int{"src/index.ts": {1}}, cov, nil)
		if want := []UncoveredFile{{Path: "src/index.ts", Ambiguous: true}}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %+v, want %+v", name, got, want)
		}
	}
}

func TestMergeCoverageCombinesFunctionsByNameAndStart(t *testing.T) {
	a := &Coverage{Format: FormatLCOV, Files: map[string]*FileCoverage{"x.ts": {Lines: map[int]int{1: 0, 5: 0}, Funcs: []FuncCoverage{{Name: "f", Start: 1, End: 2}, {Name: "f", Start: 5, End: 6}}}}}
	b := &Coverage{Format: FormatLCOV, Files: map[string]*FileCoverage{"x.ts": {Lines: map[int]int{1: 2}, Funcs: []FuncCoverage{{Name: "f", Start: 1, End: 2, Hits: 2}}}}}
	got := MergeCoverage(a, b).Files["x.ts"].Funcs
	want := []FuncCoverage{{Name: "f", Start: 1, End: 2, Hits: 2}, {Name: "f", Start: 5, End: 6}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Funcs = %+v, want %+v", got, want)
	}
}

// STA-766 review: Clover XML (PHPUnit --coverage-clover coverage.xml) also has
// a <coverage> root; it must not read as an empty, fully covered report.
func TestParseCoverageRejectsCloverAsCobertura(t *testing.T) {
	clover := `<?xml version="1.0"?><coverage generated="1"><project timestamp="1"><file name="/w/src/A.php"><line num="3" type="stmt" count="0"/></file></project></coverage>`
	if cov, err := ParseCoverage("coverage.xml", []byte(clover)); !errors.Is(err, ErrNotCoverage) {
		t.Fatalf("Clover: got cov=%+v err=%v, want ErrNotCoverage", cov, err)
	}
}
