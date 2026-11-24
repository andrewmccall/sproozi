package privatefstest_test

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/privatefs/internal/durability"
	"github.com/andrewmccall/sproozi/internal/privatefs/privatefstest"
)

const (
	module          = "github.com/andrewmccall/sproozi"
	privatefstestIP = module + "/internal/privatefs/privatefstest"
	durabilityIP    = module + "/internal/privatefs/internal/durability"
	// helper is the one file outside a test that imports privatefstest: the
	// program TestSkipSyncPanicsOutsideATestBinary runs. It is under
	// testdata, which no ./... pattern builds.
	helper = "internal/privatefs/privatefstest/testdata/outside/main.go"
)

// moduleRoot is the directory holding go.mod above this package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err = os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's directory")
		}
		dir = parent
	}
}

func goCommand(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("this guard needs the go command on PATH: %v", err)
	}
	return path
}

// durabilityImporters are the only non-test files that import durability:
// privatefs's three files that read the switch, and the file that sets it.
// Go's internal rule already limits the importers to internal/privatefs and
// below; this limits them to these four files.
var durabilityImporters = map[string]bool{
	"internal/privatefs/privatefs.go":                   true,
	"internal/privatefs/privatefs_unix.go":              true,
	"internal/privatefs/privatefs_windows.go":           true,
	"internal/privatefs/privatefstest/privatefstest.go": true,
}

// setter is the one file that may name durability.Skip.
const setter = "internal/privatefs/privatefstest/privatefstest.go"

// productionFiles parses every Go file in the repository that is not a
// test, whatever its build constraints, and passes each to visit with its
// slash-separated path from the module root. It skips directories whose
// names begin with a dot, vendor, and the top-level bin, which .gitignore
// lists as build output (bin/*); a bin directory anywhere else is source.
func productionFiles(t *testing.T, visit func(rel string, fset *token.FileSet, file *ast.File)) {
	t.Helper()
	root := moduleRoot(t)
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || rel == "bin" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution|parser.ParseComments)
		if err != nil {
			return err
		}
		visit(rel, fset, file)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 100 {
		t.Fatalf("read only %d Go files under %s; the walk is not reading the repository", files, root)
	}
}

// No file that is not a test imports privatefstest, so no production
// package can switch off privatefs's syncs. Only the three privatefs files
// that read the switch and privatefstest import durability, and of those
// only privatefstest names Skip, in any spelling: durability.Skip, a
// renamed or dot import's bare Skip, or a method value. It reads every Go
// file in the repository, whatever its build constraints, and checks import
// paths and identifiers, so an alias, a dot import or a file built only on
// another platform is still seen. The one exception to the first rule is
// the helper program TestSkipSyncPanicsOutsideATestBinary runs.
func TestOnlyTestsImportPrivatefstest(t *testing.T) {
	productionFiles(t, func(rel string, fset *token.FileSet, file *ast.File) {
		importsDurability := false
		for _, spec := range file.Imports {
			ip, _ := strconv.Unquote(spec.Path.Value)
			if ip == privatefstestIP && rel != helper {
				t.Errorf("%s imports privatefstest; only tests may", rel)
			}
			if ip == durabilityIP {
				importsDurability = true
				if !durabilityImporters[rel] {
					t.Errorf("%s imports durability; only privatefs's own files and privatefstest may", rel)
				}
			}
		}
		if !importsDurability || rel == setter {
			return
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "Skip" {
				t.Errorf("%s: %s names Skip; only privatefstest.SkipSync may", fset.Position(id.Pos()), rel)
			}
			return true
		})
	})
}

// No file that is not a test links to privatefs's switch by name. A
// //go:linkname directive reaches a variable or function in a package that
// Go's internal rule forbids importing, and needs no import, so neither the
// import checks above nor the dependency check below sees it. No directive
// in a file outside tests may mention internal/privatefs, which covers
// durability's skipped, Skip and Skipped, and none may appear in privatefs
// itself, where a one-argument directive would publish a symbol.
func TestNoLinknameReachesPrivatefs(t *testing.T) {
	productionFiles(t, func(rel string, fset *token.FileSet, file *ast.File) {
		for _, group := range file.Comments {
			for _, c := range group.List {
				if strings.Contains(c.Text, "go:linkname") &&
					(strings.Contains(c.Text, "internal/privatefs") || strings.HasPrefix(rel, "internal/privatefs/")) {
					t.Errorf("%s: %s names privatefs in a go:linkname directive; only tests may link to it", fset.Position(c.Pos()), rel)
				}
			}
		}
	})
}

// No binary the repository builds, every main package in the module,
// depends on privatefstest, or on the testing package by any route. The
// helper program under testdata is not built by ./..., and is not a
// binary the repository ships.
func TestNoBinaryDependsOnPrivatefstestOrTesting(t *testing.T) {
	root := moduleRoot(t)
	goCmd := goCommand(t)
	list := exec.Command(goCmd, "list", "-f", "{{if eq .Name \"main\"}}{{.ImportPath}}{{end}}", "./...")
	list.Dir = root
	out, err := list.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	mains := strings.Fields(string(out))
	if len(mains) < 10 {
		t.Fatalf("found only %d main packages: %v", len(mains), mains)
	}
	for _, main := range mains {
		deps := exec.Command(goCmd, "list", "-deps", main)
		deps.Dir = root
		out, err = deps.Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", main, err)
		}
		for dep := range strings.FieldsSeq(string(out)) {
			if dep == privatefstestIP || dep == "testing" {
				t.Errorf("%s depends on %s", main, dep)
			}
		}
	}
}

// SkipSync refuses to run outside a test binary: an ordinary program that
// calls it panics before the switch is set.
func TestSkipSyncPanicsOutsideATestBinary(t *testing.T) {
	root := moduleRoot(t)
	binary := filepath.Join(t.TempDir(), "outside")
	build := exec.Command(goCommand(t), "build", "-o", binary, "./"+filepath.Dir(helper))
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", helper, err, out)
	}
	out, err := exec.Command(binary).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || !strings.Contains(string(out), "privatefstest: SkipSync called outside a test binary") ||
		strings.Contains(string(out), "returned") {
		t.Fatalf("outside a test binary SkipSync gave %v:\n%s", err, out)
	}
}

// With syncs skipped, Replace still makes every check: it writes a private
// file, refuses a name that is not an ordinary file name, refuses a target
// that is a symbolic link, and leaves no temporary file behind.
func TestSkipSyncKeepsEveryCheck(t *testing.T) {
	privatefstest.SkipSync()
	if !durability.Skipped() {
		t.Fatal("SkipSync did not set the switch")
	}
	parent, err := os.MkdirTemp("", "privatefstest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	dir := filepath.Join(parent, "private")
	if err = privatefs.EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	if err = privatefs.Replace(dir, "record", []byte("one\n")); err != nil {
		t.Fatal(err)
	}
	if err = privatefs.Check(filepath.Join(dir, "record"), false); err != nil {
		t.Errorf("written file is not private: %v", err)
	}
	if got, err := privatefs.ReadFile(filepath.Join(dir, "record"), 16); err != nil || string(got) != "one\n" {
		t.Errorf("read back %q, %v", got, err)
	}
	if err = privatefs.Replace(dir, "../record", nil); err == nil {
		t.Error("accepted a name outside the directory")
	}
	if err = os.Symlink(filepath.Join(parent, "elsewhere"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err = privatefs.Replace(dir, "link", []byte("x")); err == nil {
		t.Error("replaced a symbolic link")
	}
	if err = privatefs.SyncDir(dir); err != nil {
		t.Errorf("SyncDir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pending-") {
			t.Errorf("left %s behind", e.Name())
		}
	}
}
