// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// gateFixture is the source every gate test below runs over. It declares a
// header, so its bindable boundary is the DECLARED output and the assertions
// below can name it.
const gateFixture = "four-kind.flow"

// inferenceSubjectPkg is the import path of the inference fixtures' subject
// package. Its last segment is v2 and its package clause is orders, which is the
// shape a last-segment guess gets wrong.
const inferenceSubjectPkg = "example.com/inference/subject/orderpkg/v2"

// runGate runs the gate over one serialization fixture with the real subject
// module loaded.
func runGate(t *testing.T, name string) *GateResult {
	t.Helper()

	src := loadSource(t, filepath.Join("testdata", "serialization", name))
	result, err := Gate([]Source{src}, loadSerializationSubject(t), serializationPkg)
	if err != nil {
		t.Fatalf("the gate over %s failed: %v", name, err)
	}
	if result == nil {
		t.Fatal("the gate returned no result beside a nil error")
	}

	return result
}

// TestGateRunsTheAnalyzersTheRegistryDoesNotCarryAndLiftsEveryTable is the whole
// of the gate's contract in one run.
//
// THE DISCRIMINATING CONTROL IS THE SERIALIZATION CODE. Every other assertion
// here would hold for a gate that ran only All()'s twelve registered analyzers
// and produced empty tables beside a nil error. Requiring a diagnostic stamped
// with the serialization analyzer's own Name is what proves the two CONSTRUCTED
// analyzers ran, since neither is in All() and no registered analyzer can emit
// under their code.
func TestGateRunsTheAnalyzersTheRegistryDoesNotCarryAndLiftsEveryTable(t *testing.T) {
	result := runGate(t, gateFixture)

	if result.Inferred == nil {
		t.Error("the gate lifted no inferred type table")
	}
	if result.Registrations == nil {
		t.Error("the gate lifted no registration table")
	}
	if result.Boundaries == nil {
		t.Fatal("the gate lifted no boundaries")
	}

	if len(withCode(result.Diagnostics, serializationName)) == 0 {
		t.Errorf("CONTROL FAILED: no diagnostic carries the serialization analyzer's code, so the "+
			"gate ran only the registered set:\n%s", strings.Join(messages(result.Diagnostics), "\n"))
	}
}

// TestGateLiftsTheDeclaredBoundaryUnderTheFlowsOwnName pins what a consumer
// binds against.
func TestGateLiftsTheDeclaredBoundaryUnderTheFlowsOwnName(t *testing.T) {
	result := runGate(t, gateFixture)

	names, known := result.Boundaries.Names("Screening")
	if !known {
		t.Fatalf("no boundary was lifted for Screening; the gate saw %v", result.Boundaries.Flows())
	}
	if len(names) != 1 || names[0] != "ok" {
		t.Errorf("Screening's bindable outputs are %v, want [ok] — the header's declared output", names)
	}

	flows := result.Boundaries.Flows()
	if len(flows) != 1 || flows[0] != "Screening" {
		t.Errorf("the gate lifted boundaries for %v, want exactly [Screening]", flows)
	}
}

// TestBoundariesReportAbsenceRatherThanAnEmptySet pins the rule that makes an
// absent fact usable.
//
// A CALLER CANNOT TELL AN EMPTY ANSWER FROM A MISSING ONE unless the absence is
// its own signal, and the assembler REFUSES on an absent boundary while an empty
// one would read as a flow that legitimately binds nothing. The nil receiver is
// covered here too: it is the shape a caller holds after a gate that failed.
func TestBoundariesReportAbsenceRatherThanAnEmptySet(t *testing.T) {
	result := runGate(t, gateFixture)

	if names, known := result.Boundaries.Names("NoSuchFlowAnywhere"); known || names != nil {
		t.Errorf("a flow nobody exported a boundary for answered (%v, %v), want (nil, false)", names, known)
	}

	var absent *Boundaries
	if names, known := absent.Names("Screening"); known || names != nil {
		t.Errorf("a nil Boundaries answered (%v, %v), want (nil, false)", names, known)
	}
	if flows := absent.Flows(); flows != nil {
		t.Errorf("a nil Boundaries listed %v, want nil", flows)
	}
}

// TestGateLiftsOneBoundaryPerFlowNameAcrossFiles pins the dedup.
//
// A RUN COVERS MANY FILES and two of them may declare a flow of the same name.
// The boundary table is keyed by NAME, so the second declaration must not append
// a duplicate entry to the order — a caller listing Flows() would otherwise see
// one flow twice and could not tell which fact it held.
func TestGateLiftsOneBoundaryPerFlowNameAcrossFiles(t *testing.T) {
	first := loadSource(t, filepath.Join("testdata", "serialization", gateFixture))
	second := parseSource(t, "twin.flow", string(first.Src))

	result, err := Gate([]Source{first, second}, loadSerializationSubject(t), serializationPkg)
	if err != nil {
		t.Fatalf("the gate over two files failed: %v", err)
	}

	flows := result.Boundaries.Flows()
	if len(flows) != 1 || flows[0] != "Screening" {
		t.Errorf("two files declaring Screening lifted %v, want exactly [Screening]", flows)
	}
	if names, known := result.Boundaries.Names("Screening"); !known || len(names) != 1 {
		t.Errorf("Screening's boundary is (%v, %v) after two declarations, want its single declared output",
			names, known)
	}
}

// TestAnUnresolvableReferenceNamesBothScopesItWasSoughtIn pins the refusal shape
// the two real scopes require.
//
// THERE IS NO EMPTY-SCOPE FALLBACK, so when neither the generated package nor the
// .flow file's own imports holds a name, the author needs to know BOTH were
// asked. A message naming only one sends them to fix the wrong file.
func TestAnUnresolvableReferenceNamesBothScopesItWasSoughtIn(t *testing.T) {
	_, diags := inferFixture(t, "bare-unknown.flow")

	joined := strings.Join(messages(diags), "\n")
	for _, want := range []string{"in package ", "in the file's own imports, "} {
		if !strings.Contains(joined, want) {
			t.Errorf("no refusal names %q, so it reports only one of the two scopes.\ngot:\n%s", want, joined)
		}
	}
}

// TestGateRefusesANilPackageSet proves the refusal is the SHARED sentinel rather
// than a message that merely reads like one.
//
// errors.Is is the assertion because that is what a caller does. A gate that
// returned an empty result beside a nil error would be a silently degraded lane:
// every table would be absent and nothing would say why.
func TestGateRefusesANilPackageSet(t *testing.T) {
	src := loadSource(t, filepath.Join("testdata", "serialization", gateFixture))

	result, err := Gate([]Source{src}, nil, serializationPkg)
	if err == nil {
		t.Fatal("the gate accepted a nil package set")
	}
	if result != nil {
		t.Errorf("the gate refused and still returned a result: %+v", result)
	}
	if !errors.Is(err, errNoPackages) {
		t.Errorf("the refusal is %v, want it to wrap errNoPackages", err)
	}
}

// TestTheCaptureStopsRatherThanBuildingAnEmptyResult drives the capture's
// prerequisite arms directly.
//
// EACH IS A STOP, NOT A SKIP. A missing or mistyped prerequisite result means the
// driver ran the wrong analyzer, or one changed its ResultType without its
// dependants following; a capture that shrugged and returned an empty table would
// hand a caller a clean-looking answer derived from nothing. They are driven
// directly because the real driver cannot produce them — which is exactly why
// they would otherwise never be exercised.
func TestTheCaptureStopsRatherThanBuildingAnEmptyResult(t *testing.T) {
	inference := TypeInferenceAnalyzer(nil, "")
	serialization := SerializationAnalyzer(nil, "")

	stops := []struct {
		name    string
		results map[*Analyzer]any
		want    error
	}{
		{"no inferred table", map[*Analyzer]any{}, errNoInferredTypes},
		{
			"no registration table",
			map[*Analyzer]any{inference: &InferredTypes{}},
			errNoRegistrations,
		},
		{
			"no symbol table",
			map[*Analyzer]any{inference: &InferredTypes{}, serialization: &Registrations{}},
			errNoSymbols,
		},
	}

	for _, stop := range stops {
		t.Run(stop.name, func(t *testing.T) {
			out := &GateResult{Boundaries: &Boundaries{flows: map[string][]string{}}}
			err := out.capture(&Pass{ResultOf: stop.results}, inference, serialization)
			if !errors.Is(err, stop.want) {
				t.Errorf("the capture answered %v, want %v", err, stop.want)
			}
		})
	}
}

// TestAFlowWithNoExportedBoundaryIsLeftAbsentRatherThanRecordedEmpty drives the
// arm a caller reaches when the signature analyzer exported nothing for a flow.
//
// RECORDING AN EMPTY ENTRY WOULD DESTROY THE DISTINCTION Names exists to keep: an
// absent fact is refused by the assembler, and an empty one reads as a flow that
// legitimately binds nothing.
func TestAFlowWithNoExportedBoundaryIsLeftAbsentRatherThanRecordedEmpty(t *testing.T) {
	b := &Boundaries{flows: map[string][]string{}}
	b.add(&Pass{ImportFact: func(string, Fact) bool { return false }}, "Screening")

	if names, known := b.Names("Screening"); known || names != nil {
		t.Errorf("a flow with no exported fact was recorded as (%v, %v), want (nil, false)", names, known)
	}
	if flows := b.Flows(); len(flows) != 0 {
		t.Errorf("a flow with no exported fact was listed in %v", flows)
	}
}

// TestGateRefusesASourceCarryingNoParsedTree proves the driver's own refusal
// reaches a gate caller rather than being absorbed into an empty result.
func TestGateRefusesASourceCarryingNoParsedTree(t *testing.T) {
	src := loadSource(t, filepath.Join("testdata", "serialization", gateFixture))
	untreed := Source{Path: "notree.flow", Src: src.Src}

	if _, err := Gate([]Source{untreed}, loadSerializationSubject(t), serializationPkg); err == nil {
		t.Fatal("the gate accepted a source with no parsed tree")
	}
}

// TestASourceThatIsNotATransportIsReportedRatherThanLeftUntyped covers the ruling
// that a source's inferred type is its PAYLOAD.
//
// BOTH ARMS OF THE UNWRAP ARE EXERCISED, and they are different code paths: Score
// yields a NAMED struct that is simply not EdgeFactory, and Clean yields a basic
// bool that is not a named type at all. Each must be reported at its own position
// naming what it was constructed from — never silently absent, which a consumer
// cannot distinguish from a name nobody asked about.
func TestASourceThatIsNotATransportIsReportedRatherThanLeftUntyped(t *testing.T) {
	table, diags := inferFixture(t, "source-not-a-factory.flow")

	joined := strings.Join(messages(diags), "\n")
	for _, want := range []string{
		"the source structured in flow notafactory has no inferred type",
		"the source basic in flow notafactory has no inferred type",
		"is not a github.com/whitaker-io/machine/v4.EdgeFactory instantiation",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("no diagnostic contains %q.\ngot:\n%s", want, joined)
		}
	}

	// AND NEITHER NAME IS TYPED. A report beside a recorded type would leave a
	// consumer generating against the transport after all.
	for _, name := range []string{"structured", "basic"} {
		if typ, known := table.Name("notafactory", name); known {
			t.Errorf("%s was reported AND typed as %v; a refused source carries no type", name, typ)
		}
	}
}

// TestTheUnwrapMatchesTheDeclaredObjectRatherThanTheShape pins why the match is
// on the declared object.
//
// EdgeFactory IS DECLARED AS A FUNC TYPE, so a structural match would accept any
// func of that shape from any package and a text match would accept the spelling
// from any package. These are the negative arms a shape or text match would get
// wrong, driven directly because no fixture can express a foreign EdgeFactory.
func TestTheUnwrapMatchesTheDeclaredObjectRatherThanTheShape(t *testing.T) {
	pkgs := loadInferenceSubject(t)

	// THE KNOWN-POSITIVE COMES FIRST, and without it every refusal below is
	// satisfied by an unwrap that refuses everything. Listen returns the runtime's
	// own machine.EdgeFactory[T], so its result MUST unwrap — and to the datum.
	factory, err := pkgs.Resolve(inferenceSubjectPkg, "Listen[Order](\"\")")
	if err != nil {
		t.Fatalf("CONTROL FAILED: the subject's own transport factory does not resolve, so the "+
			"refusals below prove nothing: %v", err)
	}
	payload, ok := edgeFactoryPayload(factory)
	if !ok {
		t.Fatalf("CONTROL FAILED: %v was not unwrapped as an EdgeFactory instantiation", factory)
	}
	if got := payload.String(); !strings.HasSuffix(got, ".Order") {
		t.Errorf("the transport unwrapped to %s, want the subject's Order", got)
	}

	// THE REFUSALS. Two named structs and a basic string are each something a
	// source's reference can legitimately yield, and none is a transport; a shape
	// match or a text match would take at least one of them.
	for _, spelling := range []string{"Order", "Scored", "Receipt", "Order{}.ID"} {
		typ, resolveErr := pkgs.Resolve(inferenceSubjectPkg, spelling)
		if resolveErr != nil {
			t.Fatalf("CONTROL FAILED: %s does not resolve in the subject package: %v", spelling, resolveErr)
		}
		if _, unwrapped := edgeFactoryPayload(typ); unwrapped {
			t.Errorf("%s was unwrapped as an EdgeFactory instantiation", spelling)
		}
	}

	// A NIL TYPE IS NOT AN INSTANTIATION EITHER, which is the arm a caller reaches
	// when the reference resolved to nothing.
	if _, unwrapped := edgeFactoryPayload(nil); unwrapped {
		t.Error("a nil type was unwrapped as an EdgeFactory instantiation")
	}
}
