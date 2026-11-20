package harness

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// behaviourSites are the functions and declarations outside the registry that
// still branch on a managed kind. Each is behaviour waiting for the behaviour
// slice (ADR 0039 keeps behaviour in code), keyed "file: scope". A new
// harness adds cases inside these scopes without editing this list; a branch
// in any other scope fails TestNoHarnessFactOutsideTheRegistry.
var behaviourSites = map[string]string{
	"internal/harness/native_args.go: ValidateNativeArgs":    "parses Codex's -c/--config overrides",
	"internal/harness/native_args.go: NativeArgsSelectModel": "parses Codex's -m and -c model=",
	"internal/checkpoint/artifact.go: CaptureStopped":        "captures each native state format",
	"internal/checkpoint/artifact.go: nativeArtifact.native": "checks each native state format",
	"internal/checkpoint/artifact.go: Restore":               "restores each native state format",
	"cmd/native-session/launch.go: nativeAdapters":           "the supervisor adapter of each managed kind",
	"cmd/native-session/main.go: server.waitingFor":          "graceful-stop wait text from each kind's turn observation",
	"cmd/native-session/claude.go: server.claudeHook":        "Claude Code's lifecycle hooks",
}

// literalScanRoots are the packages that implement the managed harnesses, where
// a string equal to a registry value is that value repeated. Elsewhere the same
// strings name other things, such as the gateway's own key variables or a
// profile's AGENTS.md.
var literalScanRoots = []string{"internal/harness", "cmd/native-session"}

// harnessSite is one line the scan reports.
type harnessSite struct {
	scope string
	line  int
	text  string
}

// kindConstants names the constants a file declares with type NativeKind.
func kindConstants(f *ast.File) []string {
	var names []string
	for _, decl := range f.Decls {
		g, ok := decl.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, spec := range g.Specs {
			v := spec.(*ast.ValueSpec)
			if t, ok := v.Type.(*ast.Ident); ok && t.Name == "NativeKind" {
				for _, n := range v.Names {
					names = append(names, n.Name)
				}
			}
		}
	}
	return names
}

// declScope names a top-level declaration: a function, a method as
// Receiver.Name, or the variables or constants it declares.
func declScope(decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
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
	case *ast.GenDecl:
		var names []string
		for _, spec := range d.Specs {
			if v, ok := spec.(*ast.ValueSpec); ok {
				for _, n := range v.Names {
					names = append(names, n.Name)
				}
			}
		}
		return strings.Join(names, ",")
	}
	return ""
}

// kindBranches reports where f compares a value with a kind constant (== or
// !=), lists one as a case, uses one as a composite literal key or a map
// index, lists one in a []NativeKind literal, or converts a string literal to
// a NativeKind.
func kindBranches(fset *token.FileSet, f *ast.File, kinds []string) []harnessSite {
	isKind := func(e ast.Expr) bool {
		for {
			p, ok := e.(*ast.ParenExpr)
			if !ok {
				break
			}
			e = p.X
		}
		switch x := e.(type) {
		case *ast.Ident:
			return slices.Contains(kinds, x.Name)
		case *ast.SelectorExpr:
			return slices.Contains(kinds, x.Sel.Name)
		}
		return false
	}
	isKindType := func(e ast.Expr) bool {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name == "NativeKind"
		case *ast.SelectorExpr:
			return x.Sel.Name == "NativeKind"
		}
		return false
	}
	var sites []harnessSite
	for _, decl := range f.Decls {
		scope := declScope(decl)
		ast.Inspect(decl, func(n ast.Node) bool {
			var hit ast.Node
			switch x := n.(type) {
			case *ast.CompositeLit:
				if a, ok := x.Type.(*ast.ArrayType); ok && isKindType(a.Elt) && slices.ContainsFunc(x.Elts, isKind) {
					hit = x
				}
			case *ast.IndexExpr:
				if isKind(x.Index) {
					hit = x
				}
			case *ast.CallExpr:
				if len(x.Args) == 1 && isKindType(x.Fun) {
					if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						hit = x
					}
				}
			case *ast.BinaryExpr:
				if (x.Op == token.EQL || x.Op == token.NEQ) && (isKind(x.X) || isKind(x.Y)) {
					hit = x
				}
			case *ast.CaseClause:
				if slices.ContainsFunc(x.List, isKind) {
					hit = x
				}
			case *ast.KeyValueExpr:
				if isKind(x.Key) {
					hit = x
				}
			}
			if hit != nil {
				sites = append(sites, harnessSite{scope: scope, line: fset.Position(hit.Pos()).Line})
			}
			return true
		})
	}
	return sites
}

// registryValues are the registered values that harness code must read from
// the registry: credential variables, the token placeholder, instructions
// file, executable, sign-in file and MCP file name.
func registryValues() (exact, variables, files []string) {
	add := func(list *[]string, values ...string) {
		for _, v := range values {
			if v != "" && !slices.Contains(*list, v) {
				*list = append(*list, v)
			}
		}
	}
	for _, h := range registry {
		add(&exact, h.TokenPlaceholder, h.InstructionsFile, h.Executable, h.SignInFile)
		add(&variables, h.KeyEnv, h.TokenEnv)
		add(&files, h.MCP.File)
	}
	return exact, variables, files
}

// repeatedValues reports string literals in f that repeat a registry value:
// equal to one, a variable assignment such as "KEY=...", or a path ending in an
// MCP file name.
func repeatedValues(fset *token.FileSet, f *ast.File) []harnessSite {
	exact, variables, files := registryValues()
	var sites []harnessSite
	for _, decl := range f.Decls {
		scope := declScope(decl)
		ast.Inspect(decl, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if slices.Contains(exact, s) || slices.Contains(variables, s) ||
				slices.ContainsFunc(variables, func(v string) bool { return strings.HasPrefix(s, v+"=") }) ||
				slices.ContainsFunc(files, func(v string) bool { return s == v || strings.HasSuffix(s, "/"+v) }) {
				sites = append(sites, harnessSite{scope: scope, line: fset.Position(lit.Pos()).Line, text: s})
			}
			return true
		})
	}
	return sites
}

// goFiles parses the non-test, non-generated Go files under each root,
// relative to the repository, by repository path.
func goFiles(t *testing.T, fset *token.FileSet, roots ...string) map[string]*ast.File {
	t.Helper()
	files := map[string]*ast.File{}
	for _, root := range roots {
		err := filepath.WalkDir("../../"+root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
				strings.HasPrefix(d.Name(), "zz_generated") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			files[strings.TrimPrefix(filepath.ToSlash(path), "../../")] = f
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}

// A tripwire for the common form of a per-harness fact kept outside the
// registry. It fails on code under internal, cmd and api that compares a
// value with a managed kind constant (== or !=, including parenthesised),
// lists one in a case, or keys a composite literal by one, outside the
// behaviour scopes in behaviourSites; and on a string literal in the harness
// and supervisor packages that repeats a registered credential variable,
// token placeholder, instructions file, executable, sign-in file or MCP file
// name. The kind constants are every constant of type NativeKind in this
// package, so a new harness's constant is covered once declared. Comments and
// other strings are not code and are ignored.
//
// It also flags a kind constant used as a map index, one listed in a
// []NativeKind literal, and a NativeKind conversion of a string literal, such
// as NativeKind("copilot").
//
// It does not catch: a comparison with a converted string, such as
// string(kind) == "copilot"; a local constant or variable aliasing a kind
// constant and compared later; a map keyed by kind that is built or indexed
// without a kind constant; a switch on DisplayName, NativeDirectory or another
// registry value; prefix or suffix checks on kind names; registry values
// repeated outside internal/harness and cmd/native-session; and files it does
// not read: _test.go files (and their helpers), zz_generated* files, and
// everything outside internal, cmd, api and test, including verifiers under
// hack/, which list harnesses by design.
func TestNoHarnessFactOutsideTheRegistry(t *testing.T) {
	fset := token.NewFileSet()
	var kinds []string
	for name, f := range goFiles(t, fset, "internal/harness") {
		if !strings.HasPrefix(name, "internal/harness/") || strings.Count(name, "/") != 2 {
			continue
		}
		kinds = append(kinds, kindConstants(f)...)
	}
	if len(kinds) == 0 {
		t.Fatal("found no NativeKind constants in internal/harness")
	}
	seen := map[string]bool{}
	for name, f := range goFiles(t, fset, "internal", "cmd", "api", "test") {
		for _, site := range kindBranches(fset, f, kinds) {
			key := name + ": " + site.scope
			seen[key] = true
			if _, ok := behaviourSites[key]; !ok {
				t.Errorf("%s:%d (%s) branches on a managed kind; read the fact from the harness's registry entry "+
					"in %s, adding a field if it has none (docs/contributing/add-a-harness.md). Only behaviour "+
					"waiting for the behaviour slice belongs in behaviourSites in internal/harness/sites_test.go",
					name, site.line, site.scope, registryFile)
			}
		}
	}
	for key := range behaviourSites {
		if !seen[key] {
			t.Errorf("%s no longer branches on a managed kind; remove it from behaviourSites", key)
		}
	}
	for name, f := range goFiles(t, fset, literalScanRoots...) {
		if name == registryFile {
			continue
		}
		for _, site := range repeatedValues(fset, f) {
			t.Errorf("%s:%d (%s) repeats the registry value %q; read it from the harness's entry in %s",
				name, site.line, site.scope, site.text, registryFile)
		}
	}
}

// The scan flags the forms it claims to and nothing in comments or strings.
func TestHarnessSiteScanFlagsTheCommonForms(t *testing.T) {
	const harnessSource = `package harness
type NativeKind string
const (
	NativeCodex NativeKind = "codex"
	NativeAcme  NativeKind = "acme"
)
const notAKind = "copilot"
`
	fset := token.NewFileSet()
	h, err := parser.ParseFile(fset, "harness.go", harnessSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := kindConstants(h)
	if !slices.Equal(kinds, []string{"NativeCodex", "NativeAcme"}) {
		t.Fatalf("kind constants %v; want NativeCodex and NativeAcme", kinds)
	}
	for _, c := range []struct {
		name, body string
		branches   int
		literals   int
	}{
		{"a sixth kind compared with ==", `if k == harness.NativeAcme { return }`, 1, 0},
		{"!=", `if k != harness.NativeCodex { return }`, 1, 0},
		{"kind on the left", `if harness.NativeAcme == k { return }`, 1, 0},
		{"parenthesised", `if k == (harness.NativeAcme) { return }`, 1, 0},
		{"unqualified, inside the harness package", `if k == NativeAcme { return }`, 1, 0},
		{"second case value", `switch k { case "x", harness.NativeAcme: }`, 1, 0},
		{"map literal key", `_ = map[harness.NativeKind]int{harness.NativeAcme: 1}`, 1, 0},
		{"map index assignment", `m := map[harness.NativeKind]int{}; m[harness.NativeAcme] = 1`, 1, 0},
		{"kind list", `_ = slices.Contains([]harness.NativeKind{harness.NativeCodex, harness.NativeAcme}, k)`, 1, 0},
		{"kind list in its own package", `_ = []NativeKind{NativeAcme}`, 1, 0},
		{"kind from a string literal", `if k == harness.NativeKind("copilot") { return }`, 1, 0},
		{"kind from a string literal alone", `_ = harness.NativeKind("acme")`, 1, 0},
		{"converting a variable is not caught", `_ = harness.NativeKind(v)`, 0, 0},
		{"an empty kind list", `_ = []harness.NativeKind{}`, 0, 0},
		{"not caught: a local alias", `const c = harness.NativeAcme; _ = c`, 0, 0},
		{"block comment", `/* if k == harness.NativeCodex {} */`, 0, 0},
		{"line comment", `// k == harness.NativeAcme`, 0, 0},
		{"plain log string", `fmt.Println("codex: k == harness.NativeCodex")`, 0, 0},
		{"passing a kind is not a branch", `_ = harness.Managed(harness.NativeCodex)`, 0, 0},
		{"not caught: string conversion", `if string(k) == "copilot" { return }`, 0, 0},
		{"credential variable assignment", `_ = "COPILOT_GITHUB_TOKEN=" + v`, 0, 1},
		{"credential variable", `_ = os.Getenv("ANTHROPIC_API_KEY")`, 0, 1},
		{"MCP file path", `_ = "/tmp/sproozi-native/copilot-mcp.json"`, 0, 1},
		{"sign-in file", `_ = filepath.Join(home, ".credentials.json")`, 0, 1},
		{"another config.json path", `_ = "/etc/sproozi/native/config.json"`, 0, 0},
		{"an unrelated variable", `_ = "PATH=/usr/bin"`, 0, 0},
		{"an error naming a file", `_ = "copilot config.json exceeds its limit"`, 0, 0},
	} {
		src := fmt.Sprintf("package x\nfunc f(k harness.NativeKind, v string) {\n%s\n}\n", c.body)
		f, err := parser.ParseFile(fset, c.name+".go", src, parser.ParseComments)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := len(kindBranches(fset, f, kinds)); got != c.branches {
			t.Errorf("%s: %d branches; want %d", c.name, got, c.branches)
		}
		if got := len(repeatedValues(fset, f)); got != c.literals {
			t.Errorf("%s: %d repeated values; want %d", c.name, got, c.literals)
		}
	}
}
