package pki

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// structureFields are the record's fields that say which generations exist,
// which are retired and trusted, which serves, and the rotation.
var structureFields = []string{"Generations", "Serving", "Trusted", "Rotation", "Retired", "Stage", "From", "To", "PreparedAt"}

// structureWriters are the only functions that may set them: apply moves a
// record by the table; clone copies one; replaceable builds the base a
// replace applies to; sizeAfterGrowth measures a throwaway copy.
var structureWriters = []string{"apply", "clone", "replaceable", "sizeAfterGrowth"}

// ADR 0058 rule 4, observed: every operation that moves the record does so
// through apply, once, with its own event and the move the table declares,
// and operations that only issue a leaf apply nothing. A mutator that edits
// the record directly reaches the same structure without apply, and fails
// here. Guards every apply call in store.go.
func TestEveryMoveGoesThroughApply(t *testing.T) {
	roots := buildStates(t)
	type move struct {
		ev       event
		from, to Phase
	}
	var moves []move
	appliedHook = func(ev event, from, to Phase) { moves = append(moves, move{ev, from, to}) }
	defer func() { appliedHook = nil }()
	for _, tc := range []struct {
		state string
		run   func(s *Store) error
		want  []move
	}{
		{stEmpty, func(s *Store) error { return s.Initialize(testNames) }, []move{{evInitialize, phaseNone, PhaseSingle}}},
		{stSingle, func(s *Store) error { return s.PrepareRotation() }, []move{{evPrepare, PhaseSingle, PhasePrepared}}},
		{stPrepared, func(s *Store) error { return s.SwitchRotation(true) }, []move{{evSwitch, PhasePrepared, PhaseSwitched}}},
		{stSwitched, func(s *Store) error { return s.RollbackRotation() }, []move{{evRollback, PhaseSwitched, PhasePrepared}}},
		{stSwitched, func(s *Store) error { return s.RetireRotation(true) }, []move{{evRetire, PhaseSwitched, PhaseSingle}}},
		{stPrepared, func(s *Store) error { return s.ReplaceCA() }, []move{{evReplace, PhasePrepared, PhaseSingle}}},
		{stSingle, func(s *Store) error { return s.Renew() }, nil},
		{stSwitched, func(s *Store) error { _, err := s.Ensure(testNames); return err }, nil},
	} {
		moves = nil
		s := outcomeStore(t, copyStore(t, filepath.Join(roots[tc.state], "pki")))
		if err := tc.run(s); err != nil {
			t.Fatalf("from %s: %v", tc.state, err)
		}
		if !slices.Equal(moves, tc.want) {
			t.Errorf("from %s: applied %v, want %v", tc.state, moves, tc.want)
		}
	}
}

// ADR 0058 rule 4: apply is the only place a record's structure changes.
// No function in the package's code outside structureWriters assigns a
// structure field, or builds a State, Rotation or Generation literal that
// sets one, so a mutator that edits the record directly instead of applying
// its event fails here. Guards every apply call in store.go.
func TestOnlyApplyChangesTheRecordsStructure(t *testing.T) {
	entries, err := os.ReadDir(".")
	must(t, err)
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		must(t, err)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || slices.Contains(structureWriters, fn.Name.Name) {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.AssignStmt:
					for _, lhs := range n.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && slices.Contains(structureFields, sel.Sel.Name) {
							t.Errorf("%s: %s assigns %s outside apply", fset.Position(n.Pos()), fn.Name.Name, sel.Sel.Name)
						}
					}
				case *ast.CompositeLit:
					id, ok := n.Type.(*ast.Ident)
					if !ok || (id.Name != "State" && id.Name != "Rotation" && id.Name != "Generation") {
						return true
					}
					for _, elt := range n.Elts {
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							if key, ok := kv.Key.(*ast.Ident); ok && slices.Contains(structureFields, key.Name) {
								t.Errorf("%s: %s sets %s.%s outside apply", fset.Position(n.Pos()), fn.Name.Name, id.Name, key.Name)
							}
						}
					}
				}
				return true
			})
		}
	}
}
