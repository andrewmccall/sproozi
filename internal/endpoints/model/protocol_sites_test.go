package model

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// protocolTypeName is the type whose constants name the provider protocols.
const protocolTypeName = "providerProtocol"

// protocolSites are the scopes outside the stream readers that still branch
// on the provider protocol, keyed "file: scope", with how many comparisons
// each holds and why it stays a branch. Each is a one-off difference asked
// once, at a fixed place, so a method for it would only move the branch. A
// new protocol must decide each of them; any other branch on the protocol,
// or another comparison inside one of these, fails
// TestProtocolBranchesStayInTheirSites.
var protocolSites = map[string]struct {
	count  int
	reason string
}{
	"handler.go: Handler.ServeHTTP": {5, "audit service name; the OpenAI operation check, the Messages " +
		"check, the streaming allowance and the OpenAI price check, each at its own place in the order " +
		"of refusals that testdata/outcomes.txt records"},
	"handler.go: Handler.relayWhole":          {1, "a provider's non-2xx answer: OpenAI relays it, Anthropic replaces it"},
	"handler.go: Handler.copyResponseHeaders": {1, "which provider response headers reach the harness"},
	"handler.go: Handler.writeError":          {1, "the gateway's own error body in the protocol's envelope"},
	"handler.go: Handler.copyProviderHeaders": {2, "which harness headers reach the provider, and the Messages version header"},
	"handler.go: Handler.providerCost":        {1, "the price function of the protocol's usage shape"},
	"call.go: modelCall.jsonUsage":            {1, "the usage decoder of the protocol's whole JSON response"},
	// The stream readers are chosen here and nowhere else.
	"protocol.go: providerProtocol.newStream": {2, "the stream reader of each protocol"},
}

// protocolSitesDecidedFor are the protocols each of protocolSites decides.
var protocolSitesDecidedFor = []string{"openAIProtocol", "anthropicProtocol"}

// protocolConstants names the constants of type providerProtocol a file
// declares, including those that repeat the type implicitly after iota.
func protocolConstants(f *ast.File) []string {
	var names []string
	for _, decl := range f.Decls {
		g, ok := decl.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		typed := false
		for _, spec := range g.Specs {
			v := spec.(*ast.ValueSpec)
			switch t := v.Type.(type) {
			case *ast.Ident:
				typed = t.Name == protocolTypeName
			case nil:
				// An implicit repetition keeps the previous type.
				if len(v.Values) != 0 {
					typed = false
				}
			default:
				typed = false
			}
			if typed {
				for _, n := range v.Names {
					names = append(names, n.Name)
				}
			}
		}
	}
	return names
}

// scopeOf names a top-level declaration: a function, or a method as
// Receiver.Name.
func scopeOf(decl ast.Decl) string {
	d, ok := decl.(*ast.FuncDecl)
	if !ok {
		return ""
	}
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	t := d.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + d.Name.Name
	}
	return d.Name.Name
}

// parsedSources parses the package's non-test Go files.
func parsedSources(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = f
	}
	return fset, files
}

// protocolBranch is one comparison with a protocol constant, or one case
// that lists one.
type protocolBranch struct {
	site string
	line int
	// names are the constants the branch names.
	names []string
}

// protocolBranches reports where f compares a value with a protocol constant
// (== or !=) or lists one as a case, and where it asserts or switches on a
// stream reader's type.
func protocolBranches(fset *token.FileSet, file string, f *ast.File, constants []string) []protocolBranch {
	named := func(e ast.Expr) string {
		for {
			p, ok := e.(*ast.ParenExpr)
			if !ok {
				break
			}
			e = p.X
		}
		if id, ok := e.(*ast.Ident); ok && slices.Contains(constants, id.Name) {
			return id.Name
		}
		return ""
	}
	streamType := func(e ast.Expr) bool {
		if star, ok := e.(*ast.StarExpr); ok {
			e = star.X
		}
		id, ok := e.(*ast.Ident)
		return ok && (id.Name == "anthropicStream" || id.Name == "responsesStream")
	}
	var branches []protocolBranch
	for _, decl := range f.Decls {
		site := file + ": " + scopeOf(decl)
		ast.Inspect(decl, func(n ast.Node) bool {
			var names []string
			switch x := n.(type) {
			case *ast.BinaryExpr:
				if x.Op == token.EQL || x.Op == token.NEQ {
					for _, side := range []ast.Expr{x.X, x.Y} {
						if name := named(side); name != "" {
							names = append(names, name)
						}
					}
				}
			case *ast.CaseClause:
				for _, e := range x.List {
					if name := named(e); name != "" {
						names = append(names, name)
					}
				}
			case *ast.TypeAssertExpr:
				if x.Type != nil && streamType(x.Type) {
					names = append(names, "stream type")
				}
			}
			if len(names) != 0 {
				branches = append(branches, protocolBranch{site: site, line: fset.Position(n.Pos()).Line, names: names})
			}
			return true
		})
	}
	return branches
}

// TestProtocolBranchesStayInTheirSites fails when non-test code in the
// package branches on the provider protocol outside protocolSites, or adds a
// branch inside one, so the next protocol cannot be half-added: each place
// it must decide is listed. A stream reader's type is never asserted: a
// streamed call reaches its reader only through eventStream.
func TestProtocolBranchesStayInTheirSites(t *testing.T) {
	fset, files := parsedSources(t)
	var constants []string
	for _, f := range files {
		constants = append(constants, protocolConstants(f)...)
	}
	if !slices.Equal(slices.Sorted(slices.Values(constants)), slices.Sorted(slices.Values(protocolSitesDecidedFor))) {
		t.Fatalf("the protocols are %v; protocolSites was decided for %v. A new protocol decides each site, "+
			"then is added to protocolSitesDecidedFor", constants, protocolSitesDecidedFor)
	}
	counts := map[string]int{}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		for _, b := range protocolBranches(fset, name, files[name], constants) {
			if slices.Contains(b.names, "stream type") {
				t.Errorf("%s:%d asserts a stream reader's type; call it through eventStream", name, b.line)
				continue
			}
			if _, listed := protocolSites[b.site]; !listed {
				t.Errorf("%s:%d branches on the protocol (%s) outside protocolSites; put it in eventStream "+
					"or list its scope with the reason it stays", name, b.line, strings.Join(b.names, ", "))
			}
			counts[b.site]++
		}
	}
	for site, want := range protocolSites {
		if got := counts[site]; got != want.count {
			t.Errorf("%s holds %d protocol comparisons; protocolSites lists %d", site, got, want.count)
		}
	}
}

// TestEveryProtocolHasAStreamReader fails when newStream does not name each
// protocol constant, so a new protocol is not read as another's stream.
func TestEveryProtocolHasAStreamReader(t *testing.T) {
	fset, files := parsedSources(t)
	var constants []string
	for _, f := range files {
		constants = append(constants, protocolConstants(f)...)
	}
	var named []string
	for _, b := range protocolBranches(fset, "protocol.go", files["protocol.go"], constants) {
		if b.site == "protocol.go: providerProtocol.newStream" {
			named = append(named, b.names...)
		}
	}
	slices.Sort(constants)
	slices.Sort(named)
	if !slices.Equal(constants, named) {
		t.Fatalf("newStream names %v; the protocols are %v", named, constants)
	}
	for _, p := range []providerProtocol{openAIProtocol, anthropicProtocol} {
		if stream, err := p.newStream(responsesPath, eventStreamMediaType); stream == nil || err != nil {
			t.Fatalf("protocol %d has no stream reader: %v", p, err)
		}
	}
}
