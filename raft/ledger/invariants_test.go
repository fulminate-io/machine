package ledger

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
)

// applyInsideCriticalSection reports every raft append that lies INSIDE a mutex's
// critical section.
//
// THE DETECTOR IS ORDERING-SCOPED, NOT PRESENCE-BASED, and that distinction is the
// whole point. "This body contains a Lock call and an Apply call" cannot separate
// a lock HELD ACROSS the append from a lock taken, released, and only then followed
// by one — both bodies contain both selectors. A presence test therefore fires on
// legitimate code: a Save that takes a lock to read config or check a closed flag
// and then appends outside it is correct, and would be flagged.
//
// A call is inside a section when either holds:
//   - a `defer mu.Unlock()` appeared earlier in the same block, which holds the
//     section to the end of that block; or
//   - it sits lexically between a Lock/RLock call and its matching Unlock/RUnlock
//     in the same block.
//
// WHY THE INVARIANT: the durable ceiling from a serial writer is roughly two orders
// of magnitude below what concurrent producers reach, so a lock around the append
// caps every flow at the serial figure.
//
// WHAT IT CANNOT SEE: it is construction-scoped. A lock taken in a helper that the
// appending function calls is invisible to it, and that negative space is a review
// duty rather than a check.
func applyInsideCriticalSection(fset *token.FileSet, file *ast.File) []token.Position {
	var found []token.Position
	ast.Inspect(file, func(node ast.Node) bool {
		block, ok := node.(*ast.BlockStmt)
		if !ok {
			return true
		}
		found = append(found, scanBlockForHeldApply(fset, block)...)

		return true
	})

	return found
}

// scanBlockForHeldApply walks ONE block's statement list in order, tracking whether
// a lock is currently held at each statement.
func scanBlockForHeldApply(fset *token.FileSet, block *ast.BlockStmt) []token.Position {
	var found []token.Position
	deferredUnlock, lockOpen := false, false

	for _, stmt := range block.List {
		if deferred, ok := stmt.(*ast.DeferStmt); ok {
			if isMutexCall(deferred.Call, "Unlock", "RUnlock") {
				deferredUnlock = true
			}

			continue
		}
		if call := bareCall(stmt); call != nil {
			switch {
			case isMutexCall(call, "Lock", "RLock"):
				lockOpen = true

				continue
			case isMutexCall(call, "Unlock", "RUnlock"):
				lockOpen = false

				continue
			}
		}
		if deferredUnlock || lockOpen {
			found = append(found, appendCallsIn(fset, stmt)...)
		}
	}

	return found
}

// bareCall returns the call when a statement is exactly one call expression.
func bareCall(stmt ast.Stmt) *ast.CallExpr {
	expr, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return nil
	}
	call, ok := expr.X.(*ast.CallExpr)
	if !ok {
		return nil
	}

	return call
}

func isMutexCall(call *ast.CallExpr, names ...string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	return slices.Contains(names, selector.Sel.Name)
}

// appendCallsIn reports every raft append call anywhere inside a statement.
func appendCallsIn(fset *token.FileSet, stmt ast.Stmt) []token.Position {
	var found []token.Position
	ast.Inspect(stmt, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && slices.Contains([]string{"Apply", "ApplyLog"}, selector.Sel.Name) {
			found = append(found, fset.Position(call.Pos()))
		}

		return true
	})

	return found
}

// The four fixtures. The pair must vary the axis the detector discriminates on, so
// it carries TWO violating shapes and TWO legitimate ones — a three-fixture set that
// omitted the explicit-unlock violation would leave that shape undetected.
const (
	lockAcrossApplyDeferred = `package p
func (s *S) Save(b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.raft.Apply(b, 0).Error()
}
`
	lockAcrossApplyExplicit = `package p
func (s *S) Save(b []byte) error {
	s.mu.Lock()
	err := s.raft.Apply(b, 0).Error()
	s.mu.Unlock()
	return err
}
`
	appendOutsideTheLock = `package p
func (s *S) Save(b []byte) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	return s.raft.Apply(b, 0).Error()
}
`
	lockWithNoAppend = `package p
func (s *S) Get(k string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[k]
	return v, ok
}
`
)

func TestNoLedgerMethodHoldsALockAcrossARaftApply(t *testing.T) {
	fset := token.NewFileSet()
	fixture := func(name, src string) []token.Position {
		parsed, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parsing the %s fixture: %v", name, err)
		}

		return applyInsideCriticalSection(fset, parsed)
	}

	// KNOWN POSITIVES: both violating shapes must fire.
	if got := fixture("bad-deferred.go", lockAcrossApplyDeferred); len(got) != 1 {
		t.Fatalf("CONTROL FAILED: the detector found %d appends held across a deferred unlock, want 1", len(got))
	}
	if got := fixture("bad-explicit.go", lockAcrossApplyExplicit); len(got) != 1 {
		t.Fatalf("CONTROL FAILED: an append held across an EXPLICIT unlock evaded the detector (found %d, want 1)", len(got))
	}
	// KNOWN NEGATIVES: both legitimate shapes must stay silent. The first is the one
	// a presence-based detector gets wrong, and getting it wrong would leave the
	// only routes to green as weakening the detector or deleting the fixture.
	if got := fixture("good-outside.go", appendOutsideTheLock); len(got) != 0 {
		t.Fatalf("CONTROL FAILED: the detector fired on an append taken OUTSIDE the lock at %v; it is testing presence rather than ordering", got)
	}
	if got := fixture("good-nolock.go", lockWithNoAppend); len(got) != 0 {
		t.Fatalf("CONTROL FAILED: the detector fired on a lock with no append at all, at %v", got)
	}

	scanned := 0
	var offenders []token.Position
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolving the raft module root: %v", err)
	}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		parsed, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		scanned++
		offenders = append(offenders, applyInsideCriticalSection(fset, parsed)...)

		return nil
	})
	if err != nil {
		t.Fatalf("walking the raft module: %v", err)
	}
	if scanned == 0 {
		t.Fatalf("CONTROL FAILED: the walk parsed 0 Go files under %s, so a clean result is an empty walk", root)
	}

	t.Logf("scanned %d Go files under the raft module; no append is held across a lock", scanned)
	if len(offenders) != 0 {
		t.Fatalf("a raft append is held inside a critical section at %v: this serializes appends and caps the group at the serial-writer ceiling", offenders)
	}
}

func TestProductionConfigKeepsTheDefaultCompactionTriggers(t *testing.T) {
	// A production ledger: a non-empty Dir and no test tuning, which is exactly the
	// config a deployed node builds.
	l := &Ledger{
		cfg:    Config{Flow: "flow-production", LocalID: "n0", Dir: t.TempDir()},
		logger: hclog.NewNullLogger(),
		notify: make(chan bool, leadershipNotifyBuffer),
	}
	cfg := l.raftConfig()

	// THE VALUES ARE PINNED AS LITERALS RATHER THAN COMPARED TO raft.DefaultConfig().
	// Comparing the config to the source it was derived from is a check whose subject
	// supplies its own answer key: it would stay green if a library upgrade changed
	// all three underneath us, which is one of the two things this gate exists to
	// catch. The literals were read at config.go:316-330 in hashicorp/raft v1.7.3.
	const upstream = "hashicorp/raft v1.7.3 config.go:316-330"

	if cfg.SnapshotInterval != 120*time.Second {
		t.Fatalf("SnapshotInterval is %v, want 120s; either this package overrode it or %s changed", cfg.SnapshotInterval, upstream)
	}
	if cfg.SnapshotThreshold != 8192 {
		t.Fatalf("SnapshotThreshold is %d, want 8192; either this package overrode it or %s changed", cfg.SnapshotThreshold, upstream)
	}
	if cfg.TrailingLogs != 10240 {
		t.Fatalf("TrailingLogs is %d, want 10240; either this package overrode it or %s changed", cfg.TrailingLogs, upstream)
	}

	// ShutdownOnRemove joins them because Close's whole design depends on raft's
	// self-shutdown-on-removal being the stock behavior: it is handled rather than
	// flipped, by freeing the transport binding unconditionally.
	if !cfg.ShutdownOnRemove {
		t.Fatalf("ShutdownOnRemove is false; Close frees the group binding unconditionally precisely BECAUSE raft self-shuts-down on removal and discards its own future (%s)", upstream)
	}

	// CONTROL: this ledger's own settings really were applied to the same config, so
	// the assertions above read a config this package built rather than a bare
	// default that would trivially carry raft's values.
	if string(cfg.LocalID) != "n0" {
		t.Fatalf("CONTROL FAILED: the config carries LocalID %q, so it is not the one this ledger built", cfg.LocalID)
	}
	if cfg.NotifyCh == nil {
		t.Fatal("CONTROL FAILED: the config carries no NotifyCh, so it is not the one this ledger built")
	}
}
