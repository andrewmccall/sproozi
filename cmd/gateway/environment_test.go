package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const environmentPage = "../../docs/reference/required-secrets-and-config.md"

// environmentReaders are the functions that read an environment variable by
// the name in their first argument. A reader may pass its own parameter on to
// another reader.
var environmentReaders = []string{"Getenv", "LookupEnv", "envOrDefault", "durationEnv", "boundedInteger",
	"githubAppInteger"}

// tableVariable matches a variable name in backticks at the start of a row.
var tableVariable = regexp.MustCompile("^\\| `([A-Z][A-Z0-9_]*)` \\|")

// documentedEnvironment lists the variables in the first column of the table
// under "Gateway environment" on the reference page.
func documentedEnvironment(t *testing.T) []string {
	t.Helper()
	page, err := os.ReadFile(environmentPage)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	inSection := false
	for line := range strings.SplitSeq(string(page), "\n") {
		if strings.HasPrefix(line, "## ") {
			inSection = line == "## Gateway environment"
			continue
		}
		if match := tableVariable.FindStringSubmatch(line); inSection && match != nil {
			names = append(names, match[1])
		}
	}
	if len(names) == 0 {
		t.Fatalf("%s has no table under the heading \"Gateway environment\"", environmentPage)
	}
	return names
}

func TestGatewayEnvironmentTableMatchesTheCode(t *testing.T) {
	documented := documentedEnvironment(t)
	for _, name := range gatewayEnvironment {
		if !slices.Contains(documented, name) {
			t.Errorf("the gateway reads %s, which the table under \"Gateway environment\" in %s does not list",
				name, environmentPage)
		}
	}
	for _, name := range documented {
		if !slices.Contains(gatewayEnvironment, name) {
			t.Errorf("the table under \"Gateway environment\" in %s lists %s, which the gateway does not read",
				environmentPage, name)
		}
	}
	slices.Sort(documented)
	if repeated := slices.Compact(slices.Clone(documented)); len(repeated) != len(documented) {
		t.Errorf("the table lists a variable twice: %v", documented)
	}
}

// sourceFiles parses this package's non-test files and returns them with the
// values of their string constants.
func sourceFiles(t *testing.T, fset *token.FileSet) (files []*ast.File, constants map[string]string) {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	constants = map[string]string{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
		for _, decl := range file.Decls {
			if general, ok := decl.(*ast.GenDecl); ok && general.Tok == token.CONST {
				for _, spec := range general.Specs {
					collectStringConstants(spec.(*ast.ValueSpec), constants)
				}
			}
		}
	}
	return files, constants
}

func collectStringConstants(spec *ast.ValueSpec, constants map[string]string) {
	for i, identifier := range spec.Names {
		if i >= len(spec.Values) {
			continue
		}
		if literal, ok := spec.Values[i].(*ast.BasicLit); ok && literal.Kind == token.STRING {
			constants[identifier.Name], _ = strconv.Unquote(literal.Value)
		}
	}
}

// readerName is the environment reader a call invokes, or "".
func readerName(call *ast.CallExpr) string {
	var callee string
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		callee = fn.Name
	case *ast.SelectorExpr:
		if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "os" {
			callee = fn.Sel.Name
		}
	}
	if slices.Contains(environmentReaders, callee) {
		return callee
	}
	return ""
}

// Every environment read in this package's non-test files names a variable in
// gatewayEnvironment through a constant, and every variable there is read.
func TestEveryEnvironmentReadNamesAListedVariable(t *testing.T) {
	fset := token.NewFileSet()
	files, constants := sourceFiles(t, fset)
	read := map[string]bool{}
	for _, file := range files {
		for _, decl := range file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			// A reader may pass its own parameter on to another reader.
			passesThrough := slices.Contains(environmentReaders, function.Name.Name)
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 || readerName(call) == "" {
					return true
				}
				where, callee := fset.Position(call.Pos()).String(), readerName(call)
				identifier, ok := call.Args[0].(*ast.Ident)
				if !ok {
					t.Errorf("%s: %s reads a variable by something other than a named constant; "+
						"name it in environment.go and list it in gatewayEnvironment", where, callee)
					return true
				}
				value, isConstant := constants[identifier.Name]
				switch {
				case isConstant && slices.Contains(gatewayEnvironment, value):
					read[value] = true
				case isConstant:
					t.Errorf("%s: %s reads %s (%s), which is not in gatewayEnvironment",
						where, callee, identifier.Name, value)
				case !passesThrough:
					t.Errorf("%s: %s reads a variable named by %s, which is not a constant",
						where, callee, identifier.Name)
				}
				return true
			})
		}
	}
	for _, name := range gatewayEnvironment {
		if !read[name] {
			t.Errorf("gatewayEnvironment lists %s, which no code reads", name)
		}
	}
}
