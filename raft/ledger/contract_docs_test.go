package ledger

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The contract registry: which declaration must state what, in ITS OWN doc comment.
//
// PER DECLARATION, NOT PER FILE. A file-level grep is satisfied by any one
// declaration carrying the phrase, so a single labeled sentinel vouches for every
// neighbor — and the doc a consumer actually reads is the one on the declaration
// they landed on. Measured on the earlier grep form of this gate: rewording
// ErrNotLeader's own doc while Ledger.Get's kept the label left it GREEN while
// printing the claim that the file labels the refusal.
var contractDocs = map[string][]string{
	"package ledger": {"lane C2"},
	"ErrNotLeader":   {"lane C2"},
	"Ledger.Get":     {"lane C2"},
	"Store":          {"not atomic", "single-writer-per-datum", "ErrNotLeader", "lane C2"},
	"Config.Dir":     {"documented mode, not a fallback"},
}

func TestRefusalDocsLabelTheLaneC2Interim(t *testing.T) {
	docs := collectContractDocs(t)

	// CONTROL: the matcher can fail. Without it, a registry whose every lookup
	// silently returned a doc containing everything would read as a pass.
	if stated(docs["Store"], "this phrase appears in no doc comment in this package") {
		t.Fatal("CONTROL FAILED: the matcher reported a phrase that appears nowhere, so it cannot discriminate")
	}

	for declaration, phrases := range contractDocs {
		doc, ok := docs[declaration]
		if !ok {
			t.Errorf("%s: no such declaration was found in the package; the contract gate names a declaration that does not exist", declaration)

			continue
		}
		if strings.TrimSpace(doc) == "" {
			t.Errorf("%s: has no doc comment of its own, so it states none of %v", declaration, phrases)

			continue
		}
		for _, phrase := range phrases {
			if !stated(doc, phrase) {
				t.Errorf("%s: its own doc comment does not state %q; a reader landing on this declaration alone would not learn it", declaration, phrase)
			}
		}
	}
}

// stated matches case-insensitively: the step mandates that the claim be STATED,
// not the casing it is stated in.
func stated(doc, phrase string) bool {
	return strings.Contains(strings.ToLower(doc), strings.ToLower(phrase))
}

// collectContractDocs parses the package's production files and returns each
// registered declaration's OWN doc comment.
func collectContractDocs(t *testing.T) map[string]string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	fset := token.NewFileSet()
	docs := map[string]string{}
	parsed := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		parsed++
		if file.Doc != nil {
			docs["package ledger"] = file.Doc.Text()
		}
		for _, decl := range file.Decls {
			collectFromDecl(decl, docs)
		}
	}
	if parsed == 0 {
		t.Fatal("CONTROL FAILED: no production Go files were parsed, so every lookup below would be vacuously absent")
	}

	return docs
}

func collectFromDecl(decl ast.Decl, docs map[string]string) {
	switch typed := decl.(type) {
	case *ast.FuncDecl:
		if name := methodName(typed); name != "" && typed.Doc != nil {
			docs[name] = typed.Doc.Text()
		}
	case *ast.GenDecl:
		collectFromGenDecl(typed, docs)
	}
}

func methodName(decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return ""
	}
	receiver := decl.Recv.List[0].Type
	if star, ok := receiver.(*ast.StarExpr); ok {
		receiver = star.X
	}
	ident, ok := receiver.(*ast.Ident)
	if !ok {
		return ""
	}

	return ident.Name + "." + decl.Name.Name
}

// collectFromGenDecl reads the per-SPEC doc rather than the block's.
//
// A var or type block hands every spec the BLOCK's comment, which would reintroduce
// exactly the vouching problem one level down: one documented sentinel would speak
// for all four in the same block. The block's comment is used only when the block
// holds a single spec, where there is no neighbor to vouch for.
func collectFromGenDecl(decl *ast.GenDecl, docs map[string]string) {
	for _, spec := range decl.Specs {
		switch typed := spec.(type) {
		case *ast.ValueSpec:
			doc := specDoc(typed.Doc, decl)
			for _, name := range typed.Names {
				docs[name.Name] = doc
			}
		case *ast.TypeSpec:
			docs[typed.Name.Name] = specDoc(typed.Doc, decl)
			collectStructFields(typed, docs)
		}
	}
}

func specDoc(own *ast.CommentGroup, decl *ast.GenDecl) string {
	if own != nil {
		return own.Text()
	}
	if len(decl.Specs) == 1 && decl.Doc != nil {
		return decl.Doc.Text()
	}

	return ""
}

func collectStructFields(spec *ast.TypeSpec, docs map[string]string) {
	structType, ok := spec.Type.(*ast.StructType)
	if !ok || structType.Fields == nil {
		return
	}
	for _, field := range structType.Fields.List {
		if field.Doc == nil {
			continue
		}
		for _, name := range field.Names {
			docs[spec.Name.Name+"."+name.Name] = field.Doc.Text()
		}
	}
}
