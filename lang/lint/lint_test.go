// Package lint - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/analysis"
)

// astTestdata is the lang/ast corpus, read across the module boundary IN PLACE
// rather than copied. lang/analysis reads the same corpus the same way and its
// corpus_test.go says why: a copied fixture stops tracking the parser that owns
// it the moment either side moves.
var astTestdata = filepath.Join("..", "ast", "testdata")

func TestLoadWalksADirectoryRecursively(t *testing.T) {
	batch, err := Load([]string{astTestdata})
	if err != nil {
		t.Fatalf("load %s: %v", astTestdata, err)
	}
	if len(batch.Sources) == 0 {
		t.Fatal("CONTROL FAILED: the walk found no parseable sources at all")
	}
	if len(batch.Damaged) == 0 {
		t.Fatal("CONTROL FAILED: the walk found no damaged files, so the corpus is not the one this test assumes")
	}

	descended := false
	for _, src := range batch.Sources {
		if strings.Contains(src.Path, filepath.Join("testdata", "strawman")) {
			descended = true
		}
	}
	if !descended {
		t.Errorf("the walk found %d sources but none under strawman, so it did not descend", len(batch.Sources))
	}
}

func TestLoadRefusesAnEmptyOrUnreadableInput(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		names string
	}{
		{name: "no paths", paths: nil, names: "no paths named"},
		{name: "empty directory", paths: []string{t.TempDir()}, names: "no " + Extension + " sources under"},
		{name: "missing path", paths: []string{filepath.Join(t.TempDir(), "absent.flow")}, names: "cannot read"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(tc.paths)
			if err == nil {
				t.Fatal("the load reported success over an input it could not fill")
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the refusal %q does not name its cause %q", err.Error(), tc.names)
			}
		})
	}
}

func TestLoadPartitionsParseDamageAwayFromTheAnalyzers(t *testing.T) {
	broken := filepath.Join(astTestdata, "broken")
	batch, err := Load([]string{broken})
	if err != nil {
		t.Fatalf("load %s: %v", broken, err)
	}

	if len(batch.Sources) != 0 {
		t.Errorf("%d broken fixtures were handed to the analyzers, want none", len(batch.Sources))
	}
	if len(batch.Parse) == 0 {
		t.Fatal("CONTROL FAILED: the broken corpus produced no parse diagnostics at all")
	}
	if len(batch.Damaged) == 0 {
		t.Fatal("CONTROL FAILED: the broken corpus withheld no files, so the severity leg proves nothing")
	}

	for _, d := range batch.Parse {
		if d.Severity != analysis.SeverityError {
			t.Errorf("%s:%s: parse diagnostic carries severity %s, want error", d.Path, d.Pos, d.Severity)
		}
	}
}

func TestCheckLeavesTheCanonicalCorpusPassingAtTheDefaultThreshold(t *testing.T) {
	strawman := filepath.Join(astTestdata, "strawman")
	batch, err := Load([]string{strawman})
	if err != nil {
		t.Fatalf("load %s: %v", strawman, err)
	}

	result, err := Check(batch, analysis.SeverityError)
	if err != nil {
		t.Fatalf("check %s: %v", strawman, err)
	}

	// A count of zero on its own would also be what an implementation reporting
	// nothing at all produces, so the archive hint is asserted alongside it.
	archive := false
	for _, d := range result.Diagnostics {
		if strings.Contains(d.Path, "toy.flow") && strings.Contains(d.Message, "archive") {
			archive = true
			if d.Severity != analysis.SeverityHint {
				t.Errorf("toy.flow's unconsumed archive is reported at %s, want hint", d.Severity)
			}
		}
	}
	if !archive {
		t.Fatal("CONTROL FAILED: toy.flow's unconsumed archive produced no finding, so a zero here proves nothing")
	}

	if result.Failing != 0 {
		t.Errorf("the canonical corpus reports %d findings at or above error, want none", result.Failing)
	}
}

func TestCheckFailsTheRejectCorpusAndCountsByThreshold(t *testing.T) {
	rejects := filepath.Join(astTestdata, "analysis-rejects")
	batch, err := Load([]string{rejects})
	if err != nil {
		t.Fatalf("load %s: %v", rejects, err)
	}

	strict, err := Check(batch, analysis.SeverityError)
	if err != nil {
		t.Fatalf("check %s at error: %v", rejects, err)
	}
	if strict.Failing == 0 {
		t.Fatal("the reject corpus reported nothing at or above error")
	}

	wide, err := Check(batch, analysis.SeverityHint)
	if err != nil {
		t.Fatalf("check %s at hint: %v", rejects, err)
	}
	// Relative rather than pinned: the corpus belongs to lang/ast and can grow.
	if wide.Failing <= strict.Failing {
		t.Errorf("the hint threshold counts %d and the error threshold %d, want strictly more at hint",
			wide.Failing, strict.Failing)
	}
}

func TestParseThresholdAcceptsTheVocabularyAndRefusesTheRest(t *testing.T) {
	want := []analysis.Severity{analysis.SeverityError, analysis.SeverityWarning, analysis.SeverityHint}
	if len(want) != len(Thresholds) {
		t.Fatalf("CONTROL FAILED: Thresholds carries %d levels and this test expects %d; a level was added or "+
			"removed without a test, and a fourth level is how a fail-on setting turns every red green",
			len(Thresholds), len(want))
	}

	for i, name := range Thresholds {
		got, err := ParseThreshold(name)
		if err != nil {
			t.Fatalf("ParseThreshold(%q): %v", name, err)
		}
		if got != want[i] {
			t.Errorf("ParseThreshold(%q) = %v, want %v", name, got, want[i])
		}
	}

	_, err := ParseThreshold("never")
	if err == nil {
		t.Fatal("ParseThreshold accepted a level the vocabulary does not carry")
	}
	for _, name := range Thresholds {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal %q does not offer the level %q", err.Error(), name)
		}
	}
}

func TestCheckSurfacesAnAnalyzerFailure(t *testing.T) {
	batch := Batch{Sources: []analysis.Source{{Path: "untreed.flow"}}}

	_, err := Check(batch, analysis.SeverityError)
	if err == nil {
		t.Fatal("Check reported a clean run over a source carrying no parsed tree")
	}
	if !strings.Contains(err.Error(), "untreed.flow") {
		t.Errorf("the failure %q does not name the source that caused it", err.Error())
	}
}

// render loads a corpus directory, checks it at the given threshold and returns
// the text report, failing the test at the first step that could not be taken.
func render(t *testing.T, dir string, threshold analysis.Severity) (Result, string) {
	t.Helper()

	batch, err := Load([]string{dir})
	if err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	result, err := Check(batch, threshold)
	if err != nil {
		t.Fatalf("check %s: %v", dir, err)
	}

	var out strings.Builder
	if err := WriteText(&out, result); err != nil {
		t.Fatalf("write text for %s: %v", dir, err)
	}
	return result, out.String()
}

func TestWriteTextRefusesToClaimCorrectness(t *testing.T) {
	_, out := render(t, filepath.Join(astTestdata, "strawman"), analysis.SeverityError)

	for _, want := range []string{
		"0 at or above error",
		"not a proof of correctness",
		"[reachability]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not carry %q; it reads:\n%s", want, out)
		}
	}

	if strings.Contains(out, "no problems found") {
		t.Error("the report claims no problems were found, which is a proof this engine cannot supply")
	}
}

func TestWriteTextNamesEveryWithheldFile(t *testing.T) {
	result, out := render(t, filepath.Join(astTestdata, "broken"), analysis.SeverityError)

	if len(result.Damaged) == 0 {
		t.Fatal("CONTROL FAILED: the broken corpus withheld no files, so this report proves nothing")
	}
	for _, path := range result.Damaged {
		if !strings.Contains(out, path+": not analyzed") {
			t.Errorf("the report does not disclose that %s was withheld", path)
		}
	}
	if !strings.Contains(out, "cross-file findings are incomplete") {
		t.Errorf("the report withheld %d files without disclosing that the findings are incomplete:\n%s",
			len(result.Damaged), out)
	}
}

func TestWriteRulesPrintsEveryAnalyzerDocVerbatim(t *testing.T) {
	registered := analysis.All()
	if len(registered) == 0 {
		t.Fatal("CONTROL FAILED: the analyzer registry is empty, so an empty listing would pass")
	}

	var out strings.Builder
	if err := WriteRules(&out); err != nil {
		t.Fatalf("write rules: %v", err)
	}

	listing := out.String()
	for _, a := range registered {
		if !strings.Contains(listing, a.Name) {
			t.Errorf("the listing omits the analyzer %q", a.Name)
		}
		if !strings.Contains(listing, a.Doc) {
			t.Errorf("the listing does not carry %q's own Doc verbatim", a.Name)
		}
	}
}

// decodedDiagnostic is declared HERE rather than reused from the production wire
// type, so this test carries an external expectation and cannot be satisfied by
// the subject supplying its own answer key.
type decodedDiagnostic struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Col       int    `json:"col"`
	Offset    int    `json:"offset"`
	EndLine   int    `json:"end_line"`
	EndCol    int    `json:"end_col"`
	EndOffset int    `json:"end_offset"`
	Severity  string `json:"severity"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// decodedReport is the test's own reading of the document's top level.
type decodedReport struct {
	Threshold   string              `json:"threshold"`
	Failing     int                 `json:"failing"`
	Damaged     []string            `json:"damaged"`
	Diagnostics []decodedDiagnostic `json:"diagnostics"`
}

func TestWriteJSONCarriesEveryDiagnosticField(t *testing.T) {
	rejects := filepath.Join(astTestdata, "analysis-rejects")
	batch, err := Load([]string{rejects})
	if err != nil {
		t.Fatalf("load %s: %v", rejects, err)
	}
	result, err := Check(batch, analysis.SeverityError)
	if err != nil {
		t.Fatalf("check %s: %v", rejects, err)
	}
	if len(result.Diagnostics) == 0 {
		t.Fatal("CONTROL FAILED: the reject corpus produced no diagnostics, so the field legs prove nothing")
	}

	var out strings.Builder
	if err := WriteJSON(&out, result); err != nil {
		t.Fatalf("write json: %v", err)
	}

	var got decodedReport
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
		t.Fatalf("the document does not decode: %v\n%s", err, out.String())
	}

	if got.Threshold != result.Threshold.String() {
		t.Errorf("threshold %q on the wire, want %q", got.Threshold, result.Threshold.String())
	}
	if got.Failing != result.Failing {
		t.Errorf("failing %d on the wire, want %d", got.Failing, result.Failing)
	}
	if got.Damaged == nil {
		t.Error("damaged decoded as null; an empty list is one spelling of empty, not two")
	}
	if len(got.Diagnostics) != len(result.Diagnostics) {
		t.Fatalf("%d diagnostics on the wire, want %d", len(got.Diagnostics), len(result.Diagnostics))
	}

	want := result.Diagnostics[0]
	first := got.Diagnostics[0]
	mismatched := []string{}
	for _, field := range []struct {
		name string
		same bool
	}{
		{"path", first.Path == want.Path},
		{"line", first.Line == want.Pos.Line},
		{"col", first.Col == want.Pos.Col},
		{"offset", first.Offset == want.Pos.Offset},
		{"end_line", first.EndLine == want.End.Line},
		{"end_col", first.EndCol == want.End.Col},
		{"end_offset", first.EndOffset == want.End.Offset},
		{"severity", first.Severity == want.Severity.String()},
		{"code", first.Code == want.Code},
		{"message", first.Message == want.Message},
	} {
		if !field.same {
			mismatched = append(mismatched, field.name)
		}
	}
	if len(mismatched) > 0 {
		t.Errorf("the first diagnostic lost a field on the wire: %s\ngot  %+v\nwant %+v",
			strings.Join(mismatched, ", "), first, want)
	}
}

func TestLoadTakesAFileArgumentWhateverItsExtension(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(astTestdata, "strawman", "toy.flow"))
	if err != nil {
		t.Fatalf("CONTROL FAILED: the strawman could not be read: %v", err)
	}

	named := filepath.Join(t.TempDir(), "orders.txt")
	if err := os.WriteFile(named, src, 0o600); err != nil {
		t.Fatalf("write %s: %v", named, err)
	}

	batch, err := Load([]string{named})
	if err != nil {
		t.Fatalf("load %s: %v", named, err)
	}
	if len(batch.Sources) != 1 {
		t.Fatalf("Load(%q) yielded %d sources, want exactly the file named", named, len(batch.Sources))
	}
	if batch.Sources[0].Path != named {
		t.Errorf("Load(%q) yielded source %q, want the file named", named, batch.Sources[0].Path)
	}
}
