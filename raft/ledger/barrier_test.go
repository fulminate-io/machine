package ledger

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// barrierCallSites reports every call to a method named Barrier in one parsed file.
// The census below and its known-positive fixture run through THIS function, so the
// fixture proves the detector that produced the real result.
func barrierCallSites(fset *token.FileSet, file *ast.File) []string {
	var sites []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Barrier" {
			sites = append(sites, fset.Position(call.Pos()).String())
		}

		return true
	})

	return sites
}

// barrierFixture is the known positive: a function that DOES acquire raft's
// Barrier. If the detector cannot see this, a clean census means nothing.
const barrierFixture = `package fixture

func establish(r *raftLike) error {
	return r.Barrier(0).Error()
}
`

func TestNoBarrierCallExistsInThisModule(t *testing.T) {
	// CONTROL FIRST: the detector fires on a call it must catch.
	fixtureSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fixtureSet, "fixture.go", barrierFixture, 0)
	if err != nil {
		t.Fatalf("parsing the known-positive fixture: %v", err)
	}
	if found := barrierCallSites(fixtureSet, parsed); len(found) != 1 {
		t.Fatalf("CONTROL FAILED: the detector found %d Barrier calls in a fixture that makes exactly one; a clean census would prove nothing", len(found))
	}

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolving the raft module root: %v", err)
	}

	fset := token.NewFileSet()
	var scanned int
	var sites []string
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return fmt.Errorf("parsing %s: %w", path, parseErr)
		}
		scanned++
		sites = append(sites, barrierCallSites(fset, file)...)

		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking the raft module: %v", walkErr)
	}

	t.Logf("scanned %d Go files under %s", scanned, root)
	if scanned == 0 {
		t.Fatalf("CONTROL FAILED: the walk scanned 0 Go files under %s, so a clean result is an empty walk rather than evidence", root)
	}

	// raft's Barrier is a FULL REPLICATED WRITE — it appends a log entry and waits
	// for it — so a read path that acquired one would turn every read into a write.
	// The read here is built from VerifyLeader plus a wait on this state machine's
	// own applied index instead.
	if len(sites) != 0 {
		t.Fatalf("raft's Barrier is called at %s; a read must never append to the log to answer a question", strings.Join(sites, ", "))
	}
}

func TestLinearizableReadFencesAConcurrentlyCommittedWrite(t *testing.T) {
	nodes := newCluster(t, "flow-fence", 3)
	leader := waitClusterLeader(t, nodes)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A read on a NON-LEADER is refused at VerifyLeader rather than answering from
	// its own possibly-stale state machine.
	refused := 0
	for _, node := range nodes {
		if node == leader {
			continue
		}
		_, _, err := node.ledger.Get(ctx, "heap/fenced")
		if !errors.Is(err, ErrNotLeader) {
			t.Fatalf("a read on the follower %s gave %v, want ErrNotLeader", node.id, err)
		}
		refused++
	}
	if refused != 2 {
		t.Fatalf("CONTROL FAILED: only %d followers were exercised, want 2", refused)
	}

	// A concurrent writer commits a rising sequence. Every read on the leader must
	// return a value at least as new as whatever was already committed when that
	// read began — that is the linearizability claim, and a read answering from a
	// state machine lagging the commit index would break it.
	const writes = 40
	var committed atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(1); i <= writes; i++ {
			value := []byte(strconv.FormatInt(i, 10))
			if _, err := leader.ledger.Append(ctx, Entry{Kind: KindSet, Path: "heap/fenced", Value: value}); err != nil {
				t.Errorf("appending %d: %v", i, err)

				return
			}
			committed.Store(i)
		}
	}()

	reads := 0
	for committed.Load() < writes {
		before := committed.Load()
		if before == 0 {
			continue
		}
		entry, ok, err := leader.ledger.Get(ctx, "heap/fenced")
		if err != nil {
			t.Fatalf("reading on the leader while a writer commits: %v", err)
		}
		if !ok {
			t.Fatalf("heap/fenced read as absent after %d commits", before)
		}
		got, convErr := strconv.ParseInt(string(entry.Value), 10, 64)
		if convErr != nil {
			t.Fatalf("the read returned %q, which is not one of the written values: %v", entry.Value, convErr)
		}
		if got < before {
			t.Fatalf("a read returned %d while %d was already committed before that read began: the read answered from a state machine behind the commit index", got, before)
		}
		reads++
	}
	wg.Wait()

	if reads == 0 {
		t.Fatal("CONTROL FAILED: no read raced the writer, so nothing about concurrent commits was exercised")
	}
	t.Logf("%d reads on the leader each observed a value at or past what was committed before they began", reads)

	// The final read sees the last write.
	entry, ok, err := leader.ledger.Get(ctx, "heap/fenced")
	if err != nil || !ok || string(entry.Value) != strconv.Itoa(writes) {
		t.Fatalf("the final read gave %q present=%v err=%v, want %d", entry.Value, ok, err, writes)
	}
}
