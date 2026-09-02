// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
	"bytes"
	"encoding/gob"
	"testing"

	"github.com/whitaker-io/machine/lang/loader"
)

// THE LOCAL FIXTURES. Each row's witness is a value of ITS OWN type, never a
// structurally-equivalent stand-in: a codec control that measures one type while
// the derivation measures another does not share its target's path and proves
// nothing about the row it sits on.
//
// They are declared HERE rather than imported from the testdata module because gob
// keys registration on the concrete type, so an unregistered local type behaves
// identically to an unregistered imported one, and importing the fixture module
// would add a testdata module to this module's build graph for no gain.
type (
	agPlain struct{ A int }
	agMixed struct {
		A int
		C chan int
		F func()
	}
	agSealed  struct{ a int }
	agCelsius float64
	agCels    []agCelsius
	agNamed   map[string]int
	agTrio    [3]int
	agEscaped struct {
		A int
		C chan int
	}
)

// GobEncode gives agEscaped the first half of the hatch, on the value receiver.
func (e agEscaped) GobEncode() ([]byte, error) { return []byte{byte(e.A)}, nil }

// GobDecode gives it the second half, on the pointer, which is the conventional
// split and the one loader.Carries reads.
func (e *agEscaped) GobDecode(b []byte) error {
	if len(b) > 0 {
		e.A = int(b[0])
	}

	return nil
}

// agreementRow is one shape family, its declared codec behavior at both sites, and
// a witness of its own type at each.
//
// carriedIface and carriedConcrete are DECLARATIONS the run measures and checks
// before any agreement leg is consulted, so a row whose codec behavior moved fails
// on its own terms rather than being misread as a derivation defect.
type agreementRow struct {
	spelling           string
	carriedIface       bool
	carriedConcrete    bool
	divergesAtConcrete bool
	atInterface        func() error
	atConcrete         func() error
}

// interfaceWitness encodes the value through an `any`, which is the shape
// raft/ledger's encodeValue has and the shape every FrameData.Values entry takes.
func interfaceWitness[T any](value T) func() error {
	return func() error {
		var (
			sink bytes.Buffer
			slot any = value
		)

		return gob.NewEncoder(&sink).Encode(&slot)
	}
}

// concreteWitness encodes the value in a TYPED FIELD of a struct, which is what
// envelope[T].Payload is. A single shape would have hidden the asymmetry between
// the two sites entirely.
func concreteWitness[T any](value T) func() error {
	return func() error {
		var sink bytes.Buffer

		return gob.NewEncoder(&sink).Encode(struct{ Payload T }{Payload: value})
	}
}

// agreementCensus is THE CLASS, not a sample: every shape family gob treats
// differently at an interface slot, with named and unnamed twins paired so a
// regression is LOCATED rather than merely detected.
//
// Three rows exist because a reasoned rule gets them wrong. `[]int` is carried and
// `[3]int` is refused, so "the element is basic" is the wrong predicate for an
// array. `[]float64` is carried and `[]Celsius` is refused, so a slice's element
// must be tested DIRECTLY rather than through its underlying type. And `Sealed` is
// a refusal BOTH sites agree on, which keeps the divergence row from reading as
// "the concrete site never refuses anything".
func agreementCensus() []agreementRow {
	return append(carriedRows(), refusedRows()...)
}

// carriedRows are the shapes gob carries through an interface slot with no
// registration: a basic type, and a slice whose element is basic.
//
// They are the direction that makes OVER-REPORTING detectable. Without a carried
// row a contract that spoke about everything would agree with this census.
func carriedRows() []agreementRow {
	return []agreementRow{
		{
			spelling: "int", carriedIface: true, carriedConcrete: true,
			atInterface: interfaceWitness(1), atConcrete: concreteWitness(1),
		},
		{
			spelling: "string", carriedIface: true, carriedConcrete: true,
			atInterface: interfaceWitness("s"), atConcrete: concreteWitness("s"),
		},
		{
			spelling: "[]int", carriedIface: true, carriedConcrete: true,
			atInterface: interfaceWitness([]int{1}), atConcrete: concreteWitness([]int{1}),
		},
		{
			spelling: "[]float64", carriedIface: true, carriedConcrete: true,
			atInterface: interfaceWitness([]float64{1}), atConcrete: concreteWitness([]float64{1}),
		},
	}
}

// refusedRows are the shapes gob refuses at an interface slot until they are
// registered, plus the two whose concrete-site behavior is the interesting half.
func refusedRows() []agreementRow {
	return append(unnamedCompositeRows(), namedRows()...)
}

// unnamedCompositeRows reach the derivation walk through its DEFAULT arm, which is
// where the shipped false-negative class lived (finding 4da5630a). Reverting that
// repair reds exactly these five and leaves their named twins green.
func unnamedCompositeRows() []agreementRow {
	return []agreementRow{
		{
			spelling: "[3]int", carriedConcrete: true,
			atInterface: interfaceWitness([3]int{}), atConcrete: concreteWitness([3]int{}),
		},
		{
			spelling: "map[string]int", carriedConcrete: true,
			atInterface: interfaceWitness(map[string]int{}), atConcrete: concreteWitness(map[string]int{}),
		},
		{
			spelling: "[][]int", carriedConcrete: true,
			atInterface: interfaceWitness([][]int{}), atConcrete: concreteWitness([][]int{}),
		},
		{
			spelling: "[]Celsius", carriedConcrete: true,
			atInterface: interfaceWitness([]agCelsius{}), atConcrete: concreteWitness([]agCelsius{}),
		},
		{
			spelling: "[]Plain", carriedConcrete: true,
			atInterface: interfaceWitness([]agPlain{}), atConcrete: concreteWitness([]agPlain{}),
		},
	}
}

// namedRows reach the walk through its *types.Named arm. Three of them are the
// NAMED TWINS of unnamed rows above, which is what lets a regression be located.
func namedRows() []agreementRow {
	return append(namedTwinRows(), namedStructRows()...)
}

// namedTwinRows are the named counterparts of the unnamed composites, plus a named
// basic.
func namedTwinRows() []agreementRow {
	return []agreementRow{
		{
			spelling: "Trio", carriedConcrete: true,
			atInterface: interfaceWitness(agTrio{}), atConcrete: concreteWitness(agTrio{}),
		},
		{
			spelling: "NamedMap", carriedConcrete: true,
			atInterface: interfaceWitness(agNamed{}), atConcrete: concreteWitness(agNamed{}),
		},
		{
			spelling: "Cels", carriedConcrete: true,
			atInterface: interfaceWitness(agCels{}), atConcrete: concreteWitness(agCels{}),
		},
		{
			spelling: "Celsius", carriedConcrete: true,
			atInterface: interfaceWitness(agCelsius(1)), atConcrete: concreteWitness(agCelsius(1)),
		},
	}
}

// namedStructRows carry the two halves the concrete site is interesting for: the
// PLANTED DIVERGENCE and the refusal both sites agree on.
func namedStructRows() []agreementRow {
	return []agreementRow{
		{
			spelling: "Plain", carriedConcrete: true,
			atInterface: interfaceWitness(agPlain{}), atConcrete: concreteWitness(agPlain{}),
		},
		{
			// THE PLANTED DIVERGENCE. gob carries it at the concrete site with a nil
			// error and returns it with two fields gone; the contract must refuse it
			// anyway. A test asserting only agreement could not separate a structural
			// derivation from one that trusts the codec.
			spelling: "Mixed", carriedConcrete: true, divergesAtConcrete: true,
			atInterface: interfaceWitness(agMixed{}), atConcrete: concreteWitness(agMixed{}),
		},
		{
			// BOTH SITES REFUSE, which keeps the divergence row from reading as "the
			// concrete site never refuses anything".
			spelling:    "Sealed",
			atInterface: interfaceWitness(agSealed{}), atConcrete: concreteWitness(agSealed{}),
		},
		{
			spelling: "Escaped", carriedConcrete: true,
			atInterface: interfaceWitness(agEscaped{}), atConcrete: concreteWitness(agEscaped{}),
		},
	}
}

// TestEveryDerivationVerdictAgreesWithMeasuredGobAtBothSites is THE PLAN'S STANDING
// INSTRUMENT, not a one-off agreement check.
//
// THE DEFECT CLASS IT ALONE DETECTS is a false negative at the interface site: a
// declaration the contract passes clean that fails at run time. No behavioral test
// in this package can see it, because none of them looks at the codec at all. The
// loader has already shipped one instance of that class; this is what makes the
// next one visible the day it lands rather than in a generated program.
func TestEveryDerivationVerdictAgreesWithMeasuredGobAtBothSites(t *testing.T) {
	pkgs := loadSerializationSubject(t)
	deriver := loader.NewDeriver()
	rows := agreementCensus()

	carried, refused, diverged := 0, 0, 0

	for _, row := range rows {
		if row.carriedIface {
			carried++
		} else {
			refused++
		}

		if row.divergesAtConcrete {
			diverged++
		}

		checkRow(t, pkgs, deriver, row)
	}

	t.Logf("standing instrument: %d rows, %d carried and %d refused at the interface site, "+
		"%d declared concrete-site divergence", len(rows), carried, refused, diverged)

	assertCensusShape(t, len(rows), carried, refused, diverged)
}

// checkRow measures the codec, checks that measurement against the row's own
// declaration, and only then consults the derivation.
//
// THE ORDER IS THE POINT. A row whose codec behavior moved fails on its own
// control rather than being reported as a derivation defect, which is what keeps a
// changed codec from being misread as a broken contract.
func checkRow(t *testing.T, pkgs *loader.Packages, deriver *loader.Deriver, row agreementRow) {
	t.Helper()

	gobCarriesIface := row.atInterface() == nil
	if gobCarriesIface != row.carriedIface {
		t.Errorf("CONTROL FAILED: gob carried=%t for %s at the interface site, the census says %t",
			gobCarriesIface, row.spelling, row.carriedIface)

		return
	}

	gobCarriesConcrete := row.atConcrete() == nil
	if gobCarriesConcrete != row.carriedConcrete {
		t.Errorf("CONTROL FAILED: gob carried=%t for %s at the concrete site, the census says %t",
			gobCarriesConcrete, row.spelling, row.carriedConcrete)

		return
	}

	checkInterfaceAgreement(t, pkgs, deriver, row, gobCarriesIface)
	checkConcreteAgreement(t, pkgs, deriver, row, gobCarriesConcrete)
}

// checkInterfaceAgreement holds the SYMMETRIC rule: at the interface site the
// contract must speak for exactly the rows gob refuses. A silence there is a value
// that reaches the ledger and fails at run time.
func checkInterfaceAgreement(t *testing.T, pkgs *loader.Packages, deriver *loader.Deriver,
	row agreementRow, carries bool,
) {
	t.Helper()

	spoke := derivationSpeaks(t, pkgs, deriver, row.spelling, loader.SiteInterface)
	if carries == spoke {
		t.Errorf("at the INTERFACE site gob carried=%t for %s while the contract spoke=%t; "+
			"the two must be opposites there", carries, row.spelling, spoke)
	}
}

// checkConcreteAgreement holds the ASYMMETRIC rule: gob carries almost everything at
// the concrete site, including values it will silently mutilate, so where gob
// refuses the contract must speak, and where gob carries the contract may speak only
// on a row DECLARED as a divergence. An UNDECLARED divergence FAILS, which is what
// stops this gate being quietly widened to absorb a real defect later.
func checkConcreteAgreement(t *testing.T, pkgs *loader.Packages, deriver *loader.Deriver,
	row agreementRow, carries bool,
) {
	t.Helper()

	spoke := derivationSpeaks(t, pkgs, deriver, row.spelling, loader.SiteConcrete)

	if !carries && !spoke {
		t.Errorf("at the CONCRETE site gob REFUSED %s and the contract said nothing, which is a false negative",
			row.spelling)

		return
	}

	if carries && spoke && !row.divergesAtConcrete {
		t.Errorf("at the CONCRETE site the contract refused %s while gob carried it, and the census does not "+
			"declare that row a divergence", row.spelling)
	}

	if carries && !spoke && row.divergesAtConcrete {
		t.Errorf("the census declares %s a concrete-site divergence, but the contract was silent there",
			row.spelling)
	}
}

// derivationSpeaks reports whether the derivation produced any finding for the
// spelling at the site, resolved through the same loader surface the analyzer uses.
func derivationSpeaks(t *testing.T, pkgs *loader.Packages, deriver *loader.Deriver,
	spelling string, site loader.Site,
) bool {
	t.Helper()

	typ, err := pkgs.Resolve(serializationPkg, spelling)
	if err != nil {
		t.Fatalf("CONTROL FAILED: the census row %q does not resolve, so its verdict means nothing: %v",
			spelling, err)
	}

	return len(deriver.Serializable(typ, site)) > 0
}

// assertCensusShape refuses a run that could not have detected the things this gate
// exists for, before any of its numbers are believed.
func assertCensusShape(t *testing.T, rows, carried, refused, diverged int) {
	t.Helper()

	if rows < 15 {
		t.Errorf("the census carries only %d rows, which is a sample rather than the class", rows)
	}

	if carried == 0 {
		t.Error("CONTROL FAILED: no row is CARRIED at the interface site, so over-reporting cannot be detected")
	}

	if refused == 0 {
		t.Error("CONTROL FAILED: no row is REFUSED at the interface site, so a false negative cannot be detected")
	}

	if diverged == 0 {
		t.Error("CONTROL FAILED: no row is declared a concrete-site divergence, so this gate cannot tell a " +
			"structural derivation from one that trusts the codec")
	}
}
