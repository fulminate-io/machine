// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"fmt"
	goast "go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
)

// THE FIXTURE CORPUS IS A BLIND SPOT FOR EVERY OTHER INSTRUMENT IN THIS REPO,
// and this file is the instrument that reaches it.
//
// The Go toolchain ignores directories named testdata, so `go list ./...` from
// this module returns only the module root and cmd/flowc — golangci-lint and any
// package-walking analyzer therefore cannot see one byte of testdata/, whatever
// their configuration says. The generated goldens carry a generated-code marker
// on top of that, a second independent exclusion. And two thirds of the corpus
// is .flow, which no Go tool parses at all.
//
// A TEST READS FILES, so none of that applies to it. This one walks the fixture
// tree, extracts the Go a .flow carries verbatim, parses it with go/parser and
// asserts a property over the parsed tree.
//
// THE PROPERTY, and the incident behind it. A machine node body that derives a
// heap cell's new value from its old one must do it in ONE Frame.Update call.
// Frame.Load followed by Frame.Save of the same cell is two operations with a
// gap between them, and the runtime dispatches every datum as its own goroutine,
// so two data interleave and one update is lost. Each call is individually
// mutex-protected inside the store, which is why the race detector stays silent
// and why the only other gate on this shape was an end-to-end run that failed
// about one time in forty.

// heapViolation is one function that writes a heap cell back after reading it,
// through two calls on the same frame.
type heapViolation struct {
	file string
	fn   string
	cell string
	line int
}

func (v heapViolation) String() string {
	return fmt.Sprintf("%s:%d: %s reads and writes heap cell %q across a Load and a Save; use one Frame.Update",
		v.file, v.line, v.fn, v.cell)
}

// fixtureScan is one walk's result. The counts are here so a walk that silently
// stopped reading files cannot be mistaken for a corpus with nothing wrong: a
// zero-violation result is only meaningful beside a non-zero span count.
type fixtureScan struct {
	violations []heapViolation
	files      int
	spans      int
	// funcSites records file -> function names actually parsed, which is what
	// proves the walk reached a NAMED file rather than merely reading some.
	funcSites map[string][]string
	// unparsed records fixtures that did not parse. testdata deliberately holds
	// invalid .flow fixtures, so this is reported rather than fatal.
	unparsed []string
}

// TestNoFixtureReadModifyWritesAHeapCellAcrossTwoCalls is the deterministic gate
// on the shape that cost this repo a multi-round flake investigation.
//
// It is strictly broader than the corpus check that encodes the same shape: it
// also catches the defect written as a METHOD and inside a function with no
// result, both of which an ast pattern keyed on a result-carrying func
// declaration cannot reach.
func TestNoFixtureReadModifyWritesAHeapCellAcrossTwoCalls(t *testing.T) {
	// THE KNOWN-POSITIVE RUNS FIRST AND THROUGH THE SAME SCANNER. Without it a
	// scanner that walked nothing, or one whose matcher never fired, would report
	// the corpus clean and read exactly like a corpus that is clean.
	planted := plantedCorpus(t)
	got := scanFixtureTree(t, planted)

	// Compared against a fixture-derived list, never a length: two sets that lost
	// the same members are still the same size.
	want := []string{
		"plant.flow: Planted (plantedCell)",
		"plant.go.txt: PlantedGo (plantedCell)",
		"plant.go.txt: PlantedMethod (plantedCell)",
		"plant.go.txt: PlantedNoResult (plantedCell)",
	}
	if diff := compareViolations(got.violations, want); diff != "" {
		t.Fatalf("the planted known-positive corpus did not read as expected, so this scanner cannot be trusted on the real one:\n%s", diff)
	}

	// The near misses are the other half of the pair: each carries the constructs
	// the scanner keys on, in a position where the shape is legitimate. One case
	// per axis the scanner claims to discriminate on — the cell, the idiom, the
	// receiver, and the arity that separates the host accessors from the frame's.
	for _, quiet := range []string{"PlantedDifferentCell", "PlantedAtomic", "PlantedOtherFrame", "PlantedHostSeed"} {
		for _, v := range got.violations {
			if v.fn == quiet {
				t.Errorf("the scanner flagged %s, which is a near miss it must stay silent on", quiet)
			}
		}
	}

	// NOW THE REAL CORPUS, through the identical function.
	shipped := scanFixtureTree(t, "testdata")

	if shipped.files < 20 || shipped.spans < 15 {
		t.Fatalf("CONTROL FAILED: the walk read %d fixture files and extracted %d Go spans from testdata; a sweep over that little is not evidence",
			shipped.files, shipped.spans)
	}

	// AN IDENTITY CHECK NEEDS AN EXTERNAL EXPECTATION. These two functions are
	// where the defect actually lived, so requiring the walk to have parsed them
	// BY NAME is what distinguishes "reached the incident sites and found them
	// clean" from "never reached them". If a fixture is renamed this fails, and
	// updating it is a deliberate act rather than a silent loss of reach.
	mustHave := map[string]string{
		filepath.Join("testdata", "e2e", "pipeline.flow"):                "Count",
		filepath.Join("testdata", "golden", "pipeline", "pipeline.flow"): "Bill",
	}
	for file, fn := range mustHave {
		if !slices.Contains(shipped.funcSites[file], fn) {
			t.Errorf("CONTROL FAILED: the walk never parsed %s out of %s, so a clean result over it means nothing (parsed there: %v)",
				fn, file, shipped.funcSites[file])
		}
	}

	for _, v := range shipped.violations {
		t.Errorf("%v", v)
	}
	if len(shipped.violations) != 0 {
		t.Fatalf("%d fixture function(s) read-modify-write a heap cell across two calls; each must become one Frame.Update",
			len(shipped.violations))
	}

	t.Logf("clean: %d fixture files, %d Go spans, %d unparsed (deliberately invalid fixtures): %v",
		shipped.files, shipped.spans, len(shipped.unparsed), shipped.unparsed)
}

// scanFixtureTree walks root for .flow, .flow.go and .go.txt fixtures and
// reports every function that Saves a heap cell it also Loaded through the same
// frame receiver.
//
// A .flow carries its Go as an opaque span, so the span is reconstructed exactly
// as the emitter reconstructs it — "func " + name + body — and parsed inside a
// synthetic package. go/parser does not resolve identifiers, so a span
// referencing types declared elsewhere parses fine.
func scanFixtureTree(t *testing.T, root string) fixtureScan {
	t.Helper()

	out := fixtureScan{funcSites: map[string][]string{}}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		switch {
		case strings.HasSuffix(name, ".flow"):
			out.scanFlow(t, path)
		case strings.HasSuffix(name, ".flow.go"), strings.HasSuffix(name, ".go.txt"):
			out.scanGo(t, path)
		default:
			return nil
		}
		out.files++

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	return out
}

// flowSpanPrologue wraps a lifted .flow span so go/parser will accept it, and
// flowSpanPrologueLines counts the lines it adds. They travel together because
// changing one without the other silently moves every reported line number.
const (
	flowSpanPrologue      = "package fixture\n\n"
	flowSpanPrologueLines = 2
)

// scanFlow extracts every verbatim func from one .flow file and inspects it.
func (s *fixtureScan) scanFlow(t *testing.T, path string) {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	file, err := ast.Parse(body)
	if err != nil {
		// testdata holds deliberately invalid fixtures. Recorded, not fatal —
		// the span floor above is what catches a walk that stopped parsing.
		s.unparsed = append(s.unparsed, path)

		return
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(ast.FuncDecl)
		if !ok {
			continue
		}
		// The emitter's own reconstruction: emit.go writes
		// "func " + fn.Name.Name + fn.Body.Text, so this is the same Go the
		// generated file receives, not an approximation of it.
		//
		// THE LINE OFFSET PUTS THE REPORT BACK ON THE .flow. The synthetic
		// prologue is two lines, so the func declaration lands on line 3 of the
		// parsed source while sitting on fn.Body.Start.Line of the fixture; the
		// difference is what every reported line is shifted by.
		s.inspectSource(t, path,
			flowSpanPrologue+"func "+fn.Name.Name+fn.Body.Text+"\n",
			fn.Body.Start.Line-flowSpanPrologueLines-1)
	}
}

// scanGo inspects a fixture that is already a whole Go file.
func (s *fixtureScan) scanGo(t *testing.T, path string) {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	s.inspectSource(t, path, string(body), 0)
}

// inspectSource parses one Go source text and records its functions and any
// violations. lineBase shifts reported lines back onto the fixture file for a
// span lifted out of a .flow.
func (s *fixtureScan) inspectSource(t *testing.T, path, source string, lineBase int) {
	t.Helper()

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, filepath.Base(path), source, parser.SkipObjectResolution)
	if err != nil {
		s.unparsed = append(s.unparsed, fmt.Sprintf("%s (%v)", path, err))

		return
	}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*goast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		s.spans++
		s.funcSites[path] = append(s.funcSites[path], fn.Name.Name)
		for _, cell := range readModifyWrittenCells(fset, fn) {
			s.violations = append(s.violations, heapViolation{
				file: path,
				fn:   fn.Name.Name,
				cell: cell.name,
				line: fset.Position(cell.pos).Line + lineBase,
			})
		}
	}
}

// cellSite is one heap cell named in a call, with where the Save happened.
type cellSite struct {
	name string
	pos  token.Pos
}

// readModifyWrittenCells returns every heap cell this function BOTH Loads and
// Saves through the same frame receiver.
//
// The two accessors are told apart from every other Load and Save in the repo by
// ARITY on the frame: Frame.Load takes the cell alone and Frame.Save takes the
// cell and a value, whereas Host().Load and Store.Save carry a context and are
// therefore one argument wider. That is what keeps a host-side seed write out of
// the result.
func readModifyWrittenCells(fset *token.FileSet, fn *goast.FuncDecl) []cellSite {
	loaded := map[string]bool{}
	saved := map[string]token.Pos{}

	// The walk covers the whole body including any closure inside it, because a
	// node body that hands the read-modify-write to a func literal has the same
	// defect in a different wrapper.
	goast.Inspect(fn.Body, func(n goast.Node) bool {
		call, ok := n.(*goast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*goast.SelectorExpr)
		if !ok {
			return true
		}
		switch {
		case sel.Sel.Name == "Load" && len(call.Args) == 1:
			loaded[frameCellKey(fset, sel.X, call.Args[0])] = true
		case sel.Sel.Name == "Save" && len(call.Args) == 2:
			saved[frameCellKey(fset, sel.X, call.Args[0])] = call.Pos()
		}

		return true
	})

	var out []cellSite
	for key, pos := range saved {
		if !loaded[key] {
			continue
		}
		_, cell, _ := strings.Cut(key, "\x00")
		out = append(out, cellSite{name: cell, pos: pos})
	}
	slices.SortFunc(out, func(a, b cellSite) int { return strings.Compare(a.name, b.name) })

	return out
}

// frameCellKey pairs the receiver's source text with the cell's, so a Load on
// one frame and a Save on another are never matched to each other.
func frameCellKey(fset *token.FileSet, recv, cell goast.Expr) string {
	return exprText(fset, recv) + "\x00" + exprText(fset, cell)
}

// exprText renders one expression back to source.
func exprText(fset *token.FileSet, e goast.Expr) string {
	var b strings.Builder
	if err := format.Node(&b, fset, e); err != nil {
		return fmt.Sprintf("<unrenderable %T>", e)
	}

	return b.String()
}

// compareViolations reports the difference between what a scan found and what a
// fixture-derived list says it must find.
func compareViolations(got []heapViolation, want []string) string {
	found := make([]string, 0, len(got))
	for _, v := range got {
		found = append(found, fmt.Sprintf("%s: %s (%s)", filepath.Base(v.file), v.fn, v.cell))
	}
	slices.Sort(found)
	wanted := slices.Clone(want)
	slices.Sort(wanted)
	if slices.Equal(found, wanted) {
		return ""
	}

	return fmt.Sprintf("  found: %v\n   want: %v", found, wanted)
}

// plantedCorpus writes a throwaway fixture tree carrying the defect in every
// shape the scanner claims to catch, beside one near miss per axis it claims to
// discriminate on. It is the known-positive, and it is walked by the same
// function that walks the real corpus.
func plantedCorpus(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("planting %s: %v", name, err)
		}
	}

	write("plant.flow", plantedFlow)
	write("plant.go.txt", plantedGo)

	return dir
}

// plantedFlow carries the defect in a .flow verbatim func, beside two near
// misses: one Saving a DIFFERENT cell than it Loaded, and one using the atomic
// idiom the defect should have used.
const plantedFlow = `func Planted(f machine.Frame[Order]) Order {
  n, _, err := f.Load(plantedCell)
  if err != nil {
    panic(err)
  }
  if err := f.Save(plantedCell, n+1); err != nil {
    panic(err)
  }

  return f.Value()
}

func PlantedDifferentCell(f machine.Frame[Order]) Order {
  n, _, err := f.Load(plantedCell)
  if err != nil {
    panic(err)
  }
  if err := f.Save(otherCell, n); err != nil {
    panic(err)
  }

  return f.Value()
}

func PlantedAtomic(f machine.Frame[Order]) Order {
  if _, err := f.Update(plantedCell, func(n int) int { return n + 1 }); err != nil {
    panic(err)
  }

  return f.Value()
}

flow planted
state {
  plantedCell int
  otherCell int
}
source ingest Poll()
transform plant Planted from ingest
  reads plantedCell, otherCell  writes plantedCell, otherCell
sink out Store from plant
`

// plantedGo carries the defect in a whole-Go fixture in the three shapes a
// fixture can take it: an ordinary function, a METHOD and a function with no
// result. The last two are exactly what an ast pattern keyed on a
// result-carrying func declaration cannot reach, so they are what makes this
// test broader than the corpus check rather than a duplicate of it.
//
// PlantedOtherFrame is the receiver axis: it Loads through one frame and Saves
// through another, which is not a read-modify-write of one cell.
//
// PlantedHostSeed is the ARITY axis, and it is here because the arity condition
// was otherwise exercised by nothing: no fixture in the corpus calls the host
// accessors today, so deleting both length checks left every assertion green
// while the separation they enforce was still load-bearing for the next fixture
// that seeds state the way machine_test.go already does. HostState.Load takes a
// context and a cell and HostState.Save a context, a cell and a value, so both
// are one argument wider than the frame accessors — but they share a receiver
// AND a first argument, so without the length checks they pair with each other
// on a cell named "ctx".
const plantedGo = `package plant

import (
	"context"

	machine "github.com/whitaker-io/machine/v4"
)

func PlantedGo(f machine.Frame[Order]) Order {
	n, _, _ := f.Load(plantedCell)
	_ = f.Save(plantedCell, n+1)

	return f.Value()
}

type plantedRecv struct{}

func (p *plantedRecv) PlantedMethod(f machine.Frame[Order]) Order {
	n, _, _ := f.Load(plantedCell)
	_ = f.Save(plantedCell, n+1)

	return f.Value()
}

func PlantedNoResult(f machine.Frame[Order]) {
	n, _, _ := f.Load(plantedCell)
	_ = f.Save(plantedCell, n+1)
}

func PlantedOtherFrame(f, g machine.Frame[Order]) Order {
	n, _, _ := f.Load(plantedCell)
	_ = g.Save(plantedCell, n+1)

	return f.Value()
}

func PlantedHostSeed(ctx context.Context, m *machine.Machine) {
	n, _, _ := m.Host().Load(ctx, plantedCell)
	_ = m.Host().Save(ctx, plantedCell, n+1)
}
`
