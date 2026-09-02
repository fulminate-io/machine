// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/whitaker-io/machine/lang/ast"
	"github.com/whitaker-io/machine/lang/loader"
)

// serializationName is the analyzer's Name, and therefore the Code every
// diagnostic it emits carries.
const serializationName = "serialization"

// serializationDoc is what a consumer reads at run time to learn what this
// analyzer's silence does and does not mean.
//
// FOUR OF ITS SENTENCES STATE WHAT THE SILENCE IS NOT, because that is the half
// a consumer over-reads: a clean serialization result is a statement about
// DECLARED types under ONE codec, and reading it as a proof that the program
// checkpoints safely is the mistake this Doc exists to prevent. The last two
// state the seam this analyzer sits on, where two different things are both
// spelled Path.
const serializationDoc = "serialization derives whether every Go type a flow DECLARES can cross the codec " +
	"boundary its own declaration kind reaches. A state entry is a cross-datum shared value the ledger " +
	"encodes through an interface slot; a var rides the frame, whose serializable projection is a " +
	"map[string]any; and a signature's declared input and outputs ride a checkpointed packet's payload " +
	"under their own static type. The same Go type is therefore examined at DIFFERENT sites depending on " +
	"which declaration named it, and a single boolean answer per type would be wrong. " +
	"IT IS NOT REGISTERED and never appears in All(), because it needs a caller-supplied loaded package " +
	"set and Pass has no channel through which the driver could deliver one; a caller holding the " +
	"packages builds it. Its verdict is SCOPED TO THE GOB CODEC, which is the only codec this repository " +
	"ships and the one the generator emits, so a node given some other codec is outside what this " +
	"analyzer covers and its silence says nothing about that node. A registration need is a REQUIREMENT " +
	"and not an error: the generated code emits the gob.Register call for it, and reporting it as a " +
	"refusal would make every named state type illegal. A CLEAN RESULT IS NOT A PROOF that every value " +
	"the flow checkpoints can be encoded — a node's mid-flow payload type is not declared anywhere in " +
	"the language, so it is not examined here at all, and that typing belongs to the type-flow " +
	"inference. loader.Finding.Path is a FIELD CHAIN INSIDE A TYPE such as .Inner.C while a " +
	"Diagnostic.Path is a FILESYSTEM PATH naming a source file; every diagnostic here composes the two, " +
	"taking the chain into the message and the position from the declaration that named the type. And " +
	"every position this analyzer emits is ALREADY in the .flow author's coordinate frame and must never " +
	"have a generated-file line map applied — lang/loader has two Diagnostic producers in different " +
	"frames, Errors() in the generated file's coordinates and ResolveFlow in the author's, and this " +
	"analyzer consumes neither, so no frame conversion arises here at all."

// The four declaration kinds this contract examines. They are the vocabulary a
// diagnostic names itself with, so they are the .flow author's words rather than
// this module's type names.
const (
	kindStateField = "state field"
	kindVar        = "var"
	kindOutput     = "output"
	kindInput      = "input"
)

// The five classes one loader.Reason can be routed to.
//
// EACH ARM DECLARES A DISTINCT CLASS, and that is a requirement rather than a
// nicety: four of the five report at SeverityError with zero requirements, so
// severity and the requirement count alone cannot tell them apart. Without a
// class token the contract's central claim — that each Reason reaches its OWN
// channel — would be observable only in prose.
const (
	classRequirement = "requirement"
	classSilentDrop  = "refusal-silent-drop"
	classNoExported  = "refusal-no-exported-fields"
	classDepth       = "undecided-depth-bounded"
	classUnknown     = "unknown-reason"
)

// The boundary each declaration kind's value actually crosses, traced to a read
// source line rather than reasoned. A state entry reaches raft/ledger's
// encodeValue as an `any`; a var rides FrameData.Values, which is a
// map[string]any; and a signature's declared types ride envelope[T].Payload,
// which is typed.
const (
	stateBoundary  = "is a cross-datum shared value the ledger encodes through an interface slot"
	varBoundary    = "rides the frame, whose serializable projection FrameData.Values is a map[string]any"
	outputBoundary = "rides a checkpointed packet, whose payload the codec marshals under its own static type"
	inputBoundary  = "rides the implicit ingress checkpoint's packet, whose payload the codec marshals " +
		"under its own static type"
)

// memoSep separates a memo key's two components. It is a byte no type spelling
// can contain, so no pair of distinct (spelling, site) keys can collide by
// concatenation.
const memoSep = "\x00"

// Registration is one gob.Register call the generated code must emit, and the
// declaration that made it necessary.
//
// IT IS DATA FOR A CONSUMER THAT DOES NOT EXIST YET. The generation driver reads
// this table to emit registrations, so it carries the declaration's identity
// (Flow, Name, Kind), the spelling the author wrote, the type the derivation
// actually named, and the position to blame if the emission is wrong.
type Registration struct {
	Flow     string
	Name     string
	Kind     string
	Spelling string
	Type     string
	Pos      ast.Position
}

// Registrations is this analyzer's result: every registration the run found to
// be required, in the order the run found them.
//
// A REQUIREMENT IS NOT A DIAGNOSTIC'S SUBJECT. The hint reported alongside each
// entry tells an author what the generator will do; this table is what the
// generator reads.
type Registrations struct {
	Required []Registration
}

// resolution is one (spelling, site) question's answer, cached for the run.
//
// IT CACHES THE FINDINGS AND THE ERROR, never the resolved types.Type. A memo
// holding the type would make this package's cache a second home for go/types
// values, and the findings are the whole of what the reporting needs.
type resolution struct {
	found []loader.Finding
	err   error
}

// declaredType is one declaration that names a Go type: whose it is, what it
// spelled, which boundary its value crosses, and where to report.
type declaredType struct {
	flow     string
	kind     string
	name     string
	spelling string
	boundary string
	site     loader.Site
	pos      ast.Position
	end      ast.Position
}

// verdict is the channel one loader.Reason is routed to.
type verdict struct {
	class    string
	severity Severity
	requires bool
}

// serializationRun is one analyzer's run-scoped state.
//
// IT IS A RECEIVER RATHER THAN A THREADED PARAMETER LIST because the walk needs
// the package set, the package path, one deriver, the memo and both counters —
// and the pinned linter's argument limit is five.
//
// ONE DERIVER AND ONE MEMO SERVE THE WHOLE RUN, which is what makes the cost
// shape one resolution-and-walk per (spelling, site) rather than one per
// declaration. A corpus declaring one type in forty places pays for it once.
type serializationRun struct {
	pkgs         *loader.Packages
	pkgPath      string
	deriver      *loader.Deriver
	resolved     map[string]resolution
	required     []Registration
	declarations int
	resolutions  int
}

// SerializationAnalyzer reports whether every Go type a flow declares can cross
// the codec boundary its declaration kind reaches, and tables the gob
// registrations the generated code must emit.
//
// IT IS CONSTRUCTED RATHER THAN REGISTERED, and that is structural rather than a
// preference: Pass carries six fields and the driver builds every Pass itself,
// so there is no channel through which a registered analyzer could receive a
// caller-supplied *loader.Packages. It must never be added to analyzers.go.
//
// pkgPath names the package every declared spelling is resolved against, which
// is the generated package for the .flow files under analysis.
func SerializationAnalyzer(pkgs *loader.Packages, pkgPath string) *Analyzer {
	return newSerializationRun(pkgs, pkgPath).analyzer()
}

// newSerializationRun builds the run-scoped state one analyzer holds for its
// whole life.
func newSerializationRun(pkgs *loader.Packages, pkgPath string) *serializationRun {
	return &serializationRun{
		pkgs:     pkgs,
		pkgPath:  pkgPath,
		deriver:  loader.NewDeriver(),
		resolved: map[string]resolution{},
	}
}

// analyzer wraps the run in the Analyzer the driver runs.
func (r *serializationRun) analyzer() *Analyzer {
	return &Analyzer{
		Name:       serializationName,
		Doc:        serializationDoc,
		Requires:   []*Analyzer{SymbolsAnalyzer},
		Run:        r.run,
		ResultType: reflect.TypeOf((*Registrations)(nil)),
	}
}

// run examines every flow in the run and hands back the registrations it found.
//
// A NIL PACKAGE SET IS REFUSED rather than reported as a clean program. The
// refusal WRAPS the shared errNoPackages sentinel so errors.Is holds for a
// caller comparing it while the message still names the analysis that stopped.
func (r *serializationRun) run(p *Pass) (any, error) {
	if r.pkgs == nil {
		return nil, fmt.Errorf("serialization derivation: %w", errNoPackages)
	}

	table, ok := p.ResultOf[SymbolsAnalyzer].(*SymbolTable)
	if !ok {
		return nil, errNoSymbols
	}

	for f := range table.Files {
		src := table.Files[f].Src
		for i := range table.Files[f].Flows {
			r.examineFlow(p, src, &table.Files[f].Flows[i])
		}
	}

	return &Registrations{Required: r.required}, nil
}

// examineFlow examines every declaration in one flow that names a Go type.
func (r *serializationRun) examineFlow(p *Pass, src Source, flow *FlowSymbols) {
	for _, decl := range boundaryCrossings(flow) {
		r.examine(p, src, decl)
	}
}

// examine resolves one declaration's spelling and reports what the derivation
// found, or reports that the spelling names no Go type at all.
//
// AN UNRESOLVABLE SPELLING IS REPORTED RATHER THAN SKIPPED. Skipping it would
// make a typo read as a clean declaration, which is the silence this whole
// contract exists to remove.
func (r *serializationRun) examine(p *Pass, src Source, decl declaredType) {
	r.declarations++

	got := r.resolve(decl)
	if got.err != nil {
		p.Report(src, r.unresolved(decl, got.err))

		return
	}

	for _, found := range got.found {
		p.Report(src, r.diagnose(decl, found))
	}
}

// resolve answers one (spelling, site) question, once per run.
//
// THE KEY CARRIES THE SITE as well as the spelling, because the same type has
// different answers at the two sites: a site-blind key would hand a later
// interface question the concrete answer, the registration requirement would
// never reach the generator, and the program would fail at run time with the
// exact class this derivation exists to prevent.
func (r *serializationRun) resolve(decl declaredType) resolution {
	key := decl.spelling + memoSep + strconv.Itoa(int(decl.site))
	if got, cached := r.resolved[key]; cached {
		return got
	}

	r.resolutions++

	typ, err := r.pkgs.Resolve(r.pkgPath, decl.spelling)
	got := resolution{err: err}

	if err == nil {
		got.found = r.deriver.Serializable(typ, decl.site)
	}

	r.resolved[key] = got

	return got
}

// diagnose routes one loader.Finding to its own channel, recording a
// registration requirement when the finding is one.
//
// THE POSITION COMES FROM THE DECLARATION and the field path from the
// derivation, which is the seam this contract sits on: loader.Finding carries a
// field chain inside a type and no source position, because the walk cannot know
// which declaration named the type.
func (r *serializationRun) diagnose(decl declaredType, found loader.Finding) Diagnostic {
	routed := verdictFor(found.Reason)
	if routed.requires {
		r.required = append(r.required, Registration{
			Flow: decl.flow, Name: decl.name, Kind: decl.kind,
			Spelling: decl.spelling, Type: found.Type, Pos: decl.pos,
		})
	}

	return Diagnostic{
		Pos:      decl.pos,
		End:      decl.end,
		Message:  decl.preamble() + ", and " + reasonPhrase(found),
		Severity: routed.severity,
	}
}

// unresolved reports a declared spelling that names no Go type in the package
// this run resolves against.
func (r *serializationRun) unresolved(decl declaredType, err error) Diagnostic {
	return Diagnostic{
		Pos: decl.pos,
		End: decl.end,
		Message: decl.preamble() + ", but it does not resolve to a Go type in " +
			r.pkgPath + " — the loader reports " + err.Error(),
		Severity: SeverityError,
	}
}

// verdictFor maps one loader.Reason to the channel this contract routes it to.
//
// THE DEFAULT ARM IS MANDATORY rather than defensive. loader.Reason is an
// enumeration in a SEPARATELY VERSIONED module that has already grown a value
// once, and `exhaustive` runs repo-wide with default-signifies-exhaustive true.
// A reason this contract does not know is routed to its own class and reported,
// never silently treated as clean.
func verdictFor(reason loader.Reason) verdict {
	switch reason {
	case loader.ReasonNeedsRegistration:
		return verdict{class: classRequirement, severity: SeverityHint, requires: true}
	case loader.ReasonSilentDrop:
		return verdict{class: classSilentDrop, severity: SeverityError}
	case loader.ReasonNoExportedFields:
		return verdict{class: classNoExported, severity: SeverityError}
	case loader.ReasonDepthExceeded:
		return verdict{class: classDepth, severity: SeverityError}
	default:
		return verdict{class: classUnknown, severity: SeverityError}
	}
}

// reasonPhrase renders what the derivation found, naming the field chain inside
// the type rather than only the type.
func reasonPhrase(found loader.Finding) string {
	switch found.Reason {
	case loader.ReasonSilentDrop:
		return "the gob codec discards " + found.Type + insideType(found.Path) +
			" while reporting success, so the value arrives with that part missing and nothing at run time says so"
	case loader.ReasonNoExportedFields:
		return "the gob codec refuses " + found.Type + insideType(found.Path) +
			" outright, because it carries no field the codec is allowed to see"
	case loader.ReasonNeedsRegistration:
		return "the decoder cannot reconstruct " + found.Type + insideType(found.Path) +
			" until it is registered with encoding/gob, which the generated code emits"
	case loader.ReasonDepthExceeded:
		return depthPhrase(found)
	default:
		return unknownPhrase(found)
	}
}

// depthPhrase says the walk stopped before it could decide, naming the bound it
// stopped at through the loader's own exported constant.
//
// IT IS NOT A REFUSAL OF THE TYPE. A consumer reading it as one would report a
// legitimate type as unserializable.
func depthPhrase(found loader.Finding) string {
	return "this derivation stopped after loader.MaxDepth (" + strconv.Itoa(loader.MaxDepth) +
		") walk frames on " + found.Type + insideType(found.Path) +
		" before it could decide, so this is a statement about the walk and not a refusal of the type"
}

// unknownPhrase names a loader.Reason this contract does not know how to route.
//
// It reports the declaration as UNEXAMINED rather than clean, because a reason
// added to the loader after this contract was written is a question that was
// asked and never answered.
func unknownPhrase(found loader.Finding) string {
	return "the derivation reported reason " + strconv.Itoa(int(found.Reason)) + " on " +
		found.Type + insideType(found.Path) +
		", which this contract does not know how to route, so this declaration is unexamined rather than clean"
}

// insideType renders where inside the declared type a finding sits, which is a
// FIELD CHAIN such as .Inner.C and never a filesystem path.
func insideType(path string) string {
	if path == "" {
		return ""
	}

	return " at " + path
}

// preamble is the half of a diagnostic every declaration kind shares: which
// declaration named which spelling, and which boundary its value crosses.
func (d declaredType) preamble() string {
	return d.subject() + " declares " + d.spelling + ", which " + d.boundary
}

// subject names the declaration a diagnostic is about, in the .flow author's own
// vocabulary rather than this module's type names.
func (d declaredType) subject() string {
	return "the " + d.kind + " " + d.name + " in flow " + d.flow
}

// boundaryCrossings is every declaration in one flow that names a Go type, in a
// stable order: state entries, then vars, then the signature's input and
// outputs.
//
// IT IS NOT typeflow.go's declaredTypes, which the compiler census surfaced as a
// collision. That one maps an output NAME to its spelling for a fan-in identity
// comparison and carries no site; this one carries every declaration kind with
// the loader.Site its value actually crosses at. Same neighborhood, different
// question.
func boundaryCrossings(flow *FlowSymbols) []declaredType {
	out := stateTypes(flow)
	out = append(out, varTypes(flow)...)

	return append(out, signatureTypes(flow)...)
}

// stateTypes is every state entry's declared type, at the INTERFACE site.
//
// A state entry is a cross-datum shared value (lang/ast/ast_decl.go:11) and it
// reaches raft/ledger's encodeValue as an `any`, so it is stored under a name
// the decoder must reconstruct.
func stateTypes(flow *FlowSymbols) []declaredType {
	out := make([]declaredType, 0, len(flow.State))
	for _, name := range sortedFieldNames(flow.State) {
		field := flow.State[name]
		out = append(out, declaredType{
			flow: flow.Name, kind: kindStateField, name: name,
			spelling: declaredSpelling(field.Type), boundary: stateBoundary,
			site: loader.SiteInterface, pos: field.Type.Start, end: field.Type.Stop,
		})
	}

	return out
}

// varTypes is every var's declared type, at the INTERFACE site.
//
// A var is fresh per datum and copied per tee branch (lang/ast/ast_decl.go:50).
// It rides the frame, whose serializable projection FrameData.Values is a
// map[string]any, so every stack value is in an interface slot.
func varTypes(flow *FlowSymbols) []declaredType {
	out := make([]declaredType, 0, len(flow.Vars))
	for _, name := range sortedVarNames(flow.Vars) {
		decl := flow.Vars[name]
		out = append(out, declaredType{
			flow: flow.Name, kind: kindVar, name: name,
			spelling: declaredSpelling(decl.Type), boundary: varBoundary,
			site: loader.SiteInterface, pos: decl.Type.Start, end: decl.Type.Stop,
		})
	}

	return out
}

// signatureTypes is a signature's declared input and outputs, at the CONCRETE
// site, and nothing at all when the flow declares no signature.
//
// A FLOW WITHOUT A SIGNATURE DECLARES NO BOUNDARY TYPES, so there is nothing
// here to examine and nothing is guessed. What such a flow carries is inference
// over the flow graph, which is a different analyzer's subject.
func signatureTypes(flow *FlowSymbols) []declaredType {
	if !flow.HasSignature {
		return nil
	}

	return append([]declaredType{inputType(flow)}, outputTypes(flow)...)
}

// inputType is the declared type the implicit `in` carries.
//
// IT CROSSES ON EVERY RUN whether or not any clause says so: lang/ast/node.go
// states that implicit checkpoints at flow ingress and remote-edge boundaries
// exist regardless.
func inputType(flow *FlowSymbols) declaredType {
	return declaredType{
		flow: flow.Name, kind: kindInput, name: implicitInput,
		spelling: declaredSpelling(flow.Input), boundary: inputBoundary,
		site: loader.SiteConcrete, pos: flow.Input.Start, end: flow.Input.Stop,
	}
}

// outputTypes is every declared output's type, in declaration order.
func outputTypes(flow *FlowSymbols) []declaredType {
	out := make([]declaredType, 0, len(flow.Outputs))
	for _, output := range flow.Outputs {
		out = append(out, declaredType{
			flow: flow.Name, kind: kindOutput, name: output.Name.Name,
			spelling: declaredSpelling(output.Type), boundary: outputBoundary,
			site: loader.SiteConcrete, pos: output.Type.Start, end: output.Type.Stop,
		})
	}

	return out
}

// declaredSpelling is the Go type text a DECLARATION names.
//
// IT IS NOT inference.go's spellingOf, and the difference is a defect rather
// than a matter of style. spellingOf resolves a node REFERENCE: a span whose
// text names a func the file declares comes back as that func's LITERAL, because
// lang/ast captures FuncDecl.Body as one opaque span. A span here is a DECLARED
// TYPE, so the same input shape has the opposite correct answer — a state entry
// declared `Plain` in a file that also declares `func Plain` is the type Plain,
// and resolving it to a func literal would replace the type text with something
// that resolves to no type at all. Two functions, not one with a flag.
func declaredSpelling(span ast.GoSpan) string {
	return strings.TrimSpace(span.Text)
}

// sortedVarNames is a var table's keys in a stable order.
//
// It mirrors the shape of resolve.go's sortedFieldNames rather than
// generalising it: the two read different map value types, and one generic
// helper would be a third name for a four-line loop.
func sortedVarNames(vars map[string]ast.VarDecl) []string {
	out := make([]string, 0, len(vars))
	for name := range vars {
		out = append(out, name)
	}

	sort.Strings(out)

	return out
}
