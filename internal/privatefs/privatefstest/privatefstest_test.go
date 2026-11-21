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

// No file that is not a test imports privatefstest, so no production
// package can switch off privatefs's syncs; and no file but SkipSync's
// calls durability.Skip. It reads every Go file in the repository,
// whatever its build constraints, and checks import paths and calls, so an
// alias, a dot import or a file built only on another platform is still
// seen. Go's internal rule already limits durability's importers to
// internal/privatefs and below.
func TestOnlyTestsImportPrivatefstest(t *testing.T) {
	root := moduleRoot(t)
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "bin" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		durabilityName := ""
		for _, spec := range file.Imports {
			ip, _ := strconv.Unquote(spec.Path.Value)
			if ip == privatefstestIP && filepath.ToSlash(rel) != helper {
				t.Errorf("%s imports privatefstest; only tests may", rel)
			}
			if ip == durabilityIP {
				durabilityName = "durability"
				if spec.Name != nil {
					durabilityName = spec.Name.Name
				}
			}
		}
		if durabilityName == "" || filepath.ToSlash(rel) == "internal/privatefs/privatefstest/privatefstest.go" {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Skip" {
				if x, ok := sel.X.(*ast.Ident); ok && (x.Name == durabilityName || durabilityName == ".") {
					t.Errorf("%s: %s refers to durability.Skip; only privatefstest.SkipSync may", fset.Position(sel.Pos()), rel)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 100 {
		t.Fatalf("read only %d Go files under %s; the walk is not reading the repository", files, root)
	}
}

// No binary the repository builds, every main package under cmd and hack,
// depends on privatefstest, or on the testing package by any route.
func TestNoBinaryDependsOnPrivatefstestOrTesting(t *testing.T) {
	root := moduleRoot(t)
	goCmd := goCommand(t)
	list := exec.Command(goCmd, "list", "-f", "{{if eq .Name \"main\"}}{{.ImportPath}}{{end}}", "./cmd/...", "./hack/...")
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
