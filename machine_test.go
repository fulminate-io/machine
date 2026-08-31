// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// kv is the payload the suite carries. Its fields are exported so the frame's JSON
// projection is exercisable.
type kv struct {
	Key   string
	Count int
	Tags  []string
}

func cloneKV(v *kv) *kv {
	if v == nil {
		return nil
	}
	out := &kv{Key: v.Key, Count: v.Count}
	out.Tags = append(out.Tags, v.Tags...)
	return out
}

// NewKey and NewCell panic on a duplicate name and share one declaration
// namespace, so every handle the suite uses is declared once here with a distinct
// name. Names beginning with "namespace/" are reserved for
// TestKeyAndCellShareOneNamespace.
func identityInt(v int) int          { return v }
func identityString(v string) string { return v }

var (
	atomicCell     = NewCell[int]("seam/atomic-counter")
	roundTripKey   = NewKey("seam/round-trip", identityString)
	readMissKey    = NewKey("gate/read-miss", identityString)
	writeMissKey   = NewKey("gate/write-miss", identityString)
	heapMissCell   = NewCell[int]("gate/heap-miss")
	hasKey         = NewKey("has/zero", identityInt)
	seedCell       = NewCell[int]("heap/seed")
	teeKey         = NewKey("tee/pointer", cloneKV)
	injectedCell   = NewCell[int]("store/injected")
	chainFactorKey = NewKey("chain/factor", identityInt)
	chainSeenCell  = NewCell[int]("chain/seen")
	reclaimKey     = NewKey("reclaim/held", identityString)
	unboundKey     = NewKey("gate/unbound", identityString)
	absentCell     = NewCell[int]("heap/never-written")
	presentCell    = NewCell[int]("heap/written")
)

// countingStore is the injected Store fixture. snapshot copies under the lock and
// releases before returning, so a caller may read back through Host without
// deadlocking against this fake's own mutex.
type countingStore struct {
	mutex  sync.Mutex
	saves  int
	values map[string]any
}

func newCountingStore() *countingStore {
	return &countingStore{values: map[string]any{}}
}

func (s *countingStore) Load(path string) (any, bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	value, ok := s.values[path]
	return value, ok
}

func (s *countingStore) Save(path string, value any) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.saves++
	s.values[path] = value
}

func (s *countingStore) Update(path string, fn func(any) any) any {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	updated := fn(s.values[path])
	s.values[path] = updated
	return updated
}

func (s *countingStore) snapshot(path string) (int, any, bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	value, ok := s.values[path]
	return s.saves, value, ok
}

func startMachine(t *testing.T, ctx context.Context, m *Machine) {
	t.Helper()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func feed[T any](t *testing.T, ctx context.Context, ingest Ingest[T], count int, payload T) {
	t.Helper()
	for range count {
		if err := ingest(ctx, payload); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
}

func drain[T any](t *testing.T, out <-chan Frame[T], count int) []Frame[T] {
	t.Helper()
	got := make([]Frame[T], 0, count)
	deadline := time.After(10 * time.Second)
	for len(got) < count {
		select {
		case f := <-out:
			got = append(got, f)
		case <-deadline:
			t.Fatalf("drained %d of %d frames before the deadline", len(got), count)
		}
	}
	return got
}

func awaitError[T any](t *testing.T, errs <-chan NodeError[T]) NodeError[T] {
	t.Helper()
	select {
	case e := <-errs:
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("no node error reached a handler before the deadline")
	}
	return NodeError[T]{}
}

func awaitSignal(t *testing.T, what string, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not happen before the deadline", what)
	}
}

// pollUntil retries a condition on a deadline. Several assertions here are about
// work that completes AFTER the observable event: an instrument closes after the
// downstream send, and a frame is reclaimed after the handler is dispatched.
func pollUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not hold before the deadline", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func testCapabilities(node string, reads, writes []KeyRef) *capabilities {
	caps := &capabilities{
		node:   node,
		reads:  map[string]struct{}{},
		writes: map[string]struct{}{},
	}
	for _, ref := range reads {
		caps.reads[ref.Name()] = struct{}{}
	}
	for _, ref := range writes {
		caps.writes[ref.Name()] = struct{}{}
	}
	return caps
}

// testFrame builds a frame already bound to a node, which is what lets the seam
// tests exercise the real gated accessors without a supervisor.
func testFrame[T any](node string, payload T, store Store, reads, writes []KeyRef) Frame[T] {
	frame := newFrame(node, payload, store)
	frame.state.node = node
	frame.caps = testCapabilities(node, reads, writes)
	return frame
}

func assertPanics(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected a panic: %s", what)
		}
	}()
	fn()
}

// recoverPanic returns the value fn panicked with. assertPanics answers whether a call
// panicked; this answers with WHAT, which is what the capability assertions need — the
// package raises a *CapabilityError as the panic value so a handler can recover it
// intact, and a test that only counted the panic would not see that.
func recoverPanic(t *testing.T, what string, fn func()) any {
	t.Helper()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		fn()
	}()
	if recovered == nil {
		t.Fatalf("expected a panic: %s", what)
	}
	return recovered
}

// closableEdge is an inbound transport whose channel the test closes. It is how a node
// meets a CLOSED inbound channel rather than a cancelled context: the two are separate
// exits from the read loop and only cancellation is reachable through the machine.
type closableEdge[T any] struct{ ch chan Frame[T] }

func (*closableEdge[T]) Start(context.Context) error { return nil }

func (e *closableEdge[T]) Send(ctx context.Context, frame Frame[T]) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case e.ch <- frame:
		return nil
	}
}

func (e *closableEdge[T]) Receive() <-chan Frame[T] { return e.ch }
func (e *closableEdge[T]) Close() error             { close(e.ch); return nil }

// readLoopGoroutines counts the goroutines currently inside a worker's read loop, read
// out of a full stack dump. A read loop that RETURNS leaves nothing else observable
// from outside the package, and a count is assertable in both directions. The buffer
// grows until the dump fits, because a truncated dump would undercount and make an
// assertion of zero pass vacuously.
func readLoopGoroutines() int {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), ").readLoop(")
		}
		buf = make([]byte, 2*len(buf))
	}
}

func TestSendUnblocksOnCancel(t *testing.T) {
	edge, err := Channel[int](0)("cancel")
	if err != nil {
		t.Fatalf("channel factory: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sent := make(chan error, 1)
	go func() { sent <- edge.Send(ctx, newFrame("cancel", 1, NewMemStore())) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case got := <-sent:
		if !errors.Is(got, context.Canceled) {
			t.Fatalf("Send returned %v, want a context.Canceled error", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send never unblocked after the context was cancelled")
	}
}

func TestChannelEdgeBufferAcceptsWithoutReader(t *testing.T) {
	edge, err := Channel[int](2)("buffered")
	if err != nil {
		t.Fatalf("channel factory: %v", err)
	}
	ctx := context.Background()
	store := NewMemStore()
	accepted := make(chan error, 1)
	go func() {
		for i := 1; i <= 2; i++ {
			if failed := edge.Send(ctx, newFrame("buffered", i, store)); failed != nil {
				accepted <- failed
				return
			}
		}
		accepted <- nil
	}()

	select {
	case failed := <-accepted:
		if failed != nil {
			t.Fatalf("buffered send: %v", failed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a buffer-2 edge blocked with no reader; the per-edge buffer is not wired")
	}

	out := edge.Receive()
	first, second := <-out, <-out
	if first.Value() != 1 || second.Value() != 2 {
		t.Fatalf("received %d then %d, want 1 then 2", first.Value(), second.Value())
	}
	if err := edge.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestHeapUpdateIsAtomic(t *testing.T) {
	const writers = 200
	store := NewMemStore()
	frame := testFrame("atomic", 0, store, []KeyRef{atomicCell}, []KeyRef{atomicCell})

	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			frame.Update(atomicCell, func(n int) int { return n + 1 })
		}()
	}
	wg.Wait()

	got, ok := frame.Load(atomicCell)
	if !ok {
		t.Fatal("heap cell holds no value after concurrent updates")
	}
	if got != writers {
		t.Fatalf("heap cell holds %d after %d concurrent updates, want %d", got, writers, writers)
	}
}

func TestFrameDataRoundTripsThroughRebuild(t *testing.T) {
	store := NewMemStore()
	frame := testFrame("origin", "payload", store, []KeyRef{roundTripKey}, []KeyRef{roundTripKey})
	frame.Set(roundTripKey, "carried")

	rebuilt, err := RebuildFrame(frame.Data(), frame.Value())
	if err != nil {
		t.Fatalf("RebuildFrame refused a faithful projection: %v", err)
	}
	if rebuilt.ID() != frame.ID() {
		t.Fatalf("rebuilt frame has id %q, want %q", rebuilt.ID(), frame.ID())
	}
	if rebuilt.Source() != frame.Source() || rebuilt.Node() != frame.Node() {
		t.Fatalf("rebuilt frame reports source %q node %q, want %q and %q",
			rebuilt.Source(), rebuilt.Node(), frame.Source(), frame.Node())
	}
	rebuilt.caps = frame.caps
	if got := rebuilt.Get(roundTripKey); got != "carried" {
		t.Fatalf("rebuilt frame holds %q under the stack key, want %q", got, "carried")
	}

	undeclared := FrameData{
		ID:     frame.ID(),
		Source: "origin",
		Values: map[string]any{"seam/never-declared": 1},
	}
	if _, err := RebuildFrame(undeclared, "payload"); err == nil {
		t.Fatal("RebuildFrame accepted a projection naming an undeclared key")
	}
}

func TestKeyAndCellShareOneNamespace(t *testing.T) {
	identity := func(v int) int { return v }

	NewKey("namespace/declared-as-key", identity)
	assertPanics(t, "NewCell on a name already declared as a stack key", func() {
		NewCell[int]("namespace/declared-as-key")
	})

	NewCell[int]("namespace/declared-as-cell")
	assertPanics(t, "NewKey on a name already declared as a heap cell", func() {
		NewKey("namespace/declared-as-cell", identity)
	})

	assertPanics(t, "NewKey with an empty name", func() { NewKey("", identity) })
	assertPanics(t, "NewCell with an empty name", func() { NewCell[int]("") })
}

func TestRuntimeOwnsFramePropagation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := New("propagation")
	src, ingest := m.Source[int]("propagation.source")
	ids := make(chan string, 1)
	watched := src.Map("propagation.watch", func(f Frame[int]) int {
		ids <- f.ID()
		return f.Value()
	})
	out := watched.Map("propagation.ignores", func(Frame[int]) int { return 42 }).Output("propagation.out")

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 1)

	upstream := <-ids
	got := drain(t, out, 1)[0]
	if got.Value() != 42 {
		t.Fatalf("payload is %d, want 42", got.Value())
	}
	if got.ID() != upstream {
		t.Fatalf("frame identity changed from %q to %q; the runtime forged a frame instead of re-wrapping",
			upstream, got.ID())
	}
	if got.Source() != "propagation.source" {
		t.Fatalf("frame source is %q, want %q", got.Source(), "propagation.source")
	}
	if got.Node() != "propagation.ignores" {
		t.Fatalf("frame node stamp is %q, want %q", got.Node(), "propagation.ignores")
	}
}

func TestUndeclaredFrameReadFailsLoudly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errs := make(chan NodeError[int], 1)
	m := New("undeclared-read")
	src, ingest := m.Source[int]("undeclared-read.source")
	src.Map("undeclared-read.node", func(f Frame[int]) int {
		_ = f.Get(readMissKey)
		return f.Value()
	}, WithErrorHandler(func(e NodeError[int]) { errs <- e })).Drop("undeclared-read.drop")

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 1)

	e := awaitError(t, errs)
	if !e.Panic {
		t.Fatal("an undeclared read did not surface as a recovered panic")
	}
	var capErr *CapabilityError
	if !errors.As(e.Err, &capErr) {
		t.Fatalf("handler received %v, want a *CapabilityError", e.Err)
	}
	if capErr.Node != "undeclared-read.node" || capErr.Key != readMissKey.Name() || capErr.Access != "read" {
		t.Fatalf("CapabilityError reports node %q key %q access %q", capErr.Node, capErr.Key, capErr.Access)
	}
}

func TestUndeclaredFrameWriteFailsLoudly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errs := make(chan NodeError[int], 1)
	m := New("undeclared-write")
	src, ingest := m.Source[int]("undeclared-write.source")
	src.Map("undeclared-write.node", func(f Frame[int]) int {
		f.Set(writeMissKey, "denied")
		return f.Value()
	}, WithReads[int](writeMissKey), WithErrorHandler(func(e NodeError[int]) { errs <- e })).
		Drop("undeclared-write.drop")

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 1)

	e := awaitError(t, errs)
	var capErr *CapabilityError
	if !errors.As(e.Err, &capErr) {
		t.Fatalf("handler received %v, want a *CapabilityError", e.Err)
	}
	if capErr.Access != "write" {
		t.Fatalf("CapabilityError reports access %q, want write; a read capability must not imply a write",
			capErr.Access)
	}
	if capErr.Node != "undeclared-write.node" || capErr.Key != writeMissKey.Name() {
		t.Fatalf("CapabilityError reports node %q key %q", capErr.Node, capErr.Key)
	}
}

func TestUndeclaredHeapAccessFailsLoudly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errs := make(chan NodeError[int], 1)
	m := New("undeclared-heap")
	src, ingest := m.Source[int]("undeclared-heap.source")
	src.Map("undeclared-heap.node", func(f Frame[int]) int {
		_, _ = f.Load(heapMissCell)
		return f.Value()
	}, WithErrorHandler(func(e NodeError[int]) { errs <- e })).Drop("undeclared-heap.drop")

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 1)

	e := awaitError(t, errs)
	var capErr *CapabilityError
	if !errors.As(e.Err, &capErr) {
		t.Fatalf("handler received %v, want a *CapabilityError", e.Err)
	}
	if capErr.Node != "undeclared-heap.node" || capErr.Key != heapMissCell.Name() || capErr.Access != "read" {
		t.Fatalf("CapabilityError reports node %q key %q access %q; the heap must ride the same gate",
			capErr.Node, capErr.Key, capErr.Access)
	}
}

func TestNodeDeclaringNothingSeesOnlyValue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errs := make(chan NodeError[int], 1)
	m := New("pure")
	src, ingest := m.Source[int]("pure.source")
	out := src.Map("pure.node", func(f Frame[int]) int { return f.Value() * 2 },
		WithErrorHandler(func(e NodeError[int]) { errs <- e })).Output("pure.out")

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 21)

	got := drain(t, out, 1)[0]
	if got.Value() != 42 {
		t.Fatalf("payload is %d, want 42", got.Value())
	}
	select {
	case e := <-errs:
		t.Fatalf("a node declaring no capabilities raised %v", e.Err)
	default:
	}
}

func TestHasDistinguishesUnwrittenFromZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type observation struct {
		hasBefore, hasAfter bool
		getBefore, getAfter int
	}
	seen := make(chan observation, 1)

	m := New("has")
	src, ingest := m.Source[int]("has.source")
	src.Map("has.node", func(f Frame[int]) int {
		o := observation{hasBefore: f.Has(hasKey), getBefore: f.Get(hasKey)}
		f.Set(hasKey, 0)
		o.hasAfter, o.getAfter = f.Has(hasKey), f.Get(hasKey)
		seen <- o
		return f.Value()
	}, WithReads[int](hasKey), WithWrites[int](hasKey)).Drop("has.drop")

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 1)

	o := <-seen
	if o.hasBefore {
		t.Fatal("Has reported a never-written key as present")
	}
	if o.getBefore != 0 {
		t.Fatalf("Get on a never-written key returned %d, want the zero value", o.getBefore)
	}
	if !o.hasAfter {
		t.Fatal("Has reported a key holding the ZERO value as absent; it reports non-zero, not presence")
	}
	if o.getAfter != 0 {
		t.Fatalf("Get after writing the zero value returned %d, want 0", o.getAfter)
	}
}

func TestHeapCellRoundTripsThroughFrame(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := New("heap")
	m.Host().Save(seedCell, 7)

	loaded := make(chan int, 1)
	src, ingest := m.Source[int]("heap.source")
	src.Map("heap.node", func(f Frame[int]) int {
		got, ok := f.Load(seedCell)
		if !ok {
			got = -1
		}
		f.Save(seedCell, got*3)
		loaded <- got
		return f.Value()
	}, WithReads[int](seedCell), WithWrites[int](seedCell)).Drop("heap.drop")

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 1)

	if got := <-loaded; got != 7 {
		t.Fatalf("the node loaded %d through the frame, want the host-seeded 7", got)
	}
	host, ok := m.Host().Load(seedCell)
	if !ok {
		t.Fatal("the host reads no value back after the node wrote through the frame")
	}
	if host != 21 {
		t.Fatalf("the host reads %d, want 21", host)
	}
}

func TestTeeDeepCopyIsolatesBranches(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type observation struct {
		payloadCount int
		heldCount    int
		heldTags     int
	}
	ids := make(chan string, 1)
	mutated := make(chan struct{})
	rightSeen := make(chan observation, 1)

	m := New("tee")
	src, ingest := m.Source[*kv]("tee.source")
	watched := src.Map("tee.watch", func(f Frame[*kv]) *kv {
		f.Set(teeKey, &kv{Key: "shared", Count: 1, Tags: []string{"original"}})
		ids <- f.ID()
		return f.Value()
	}, WithWrites[*kv](teeKey))

	left, right := watched.Tee("tee.split", func(d *kv) (*kv, *kv) { return cloneKV(d), cloneKV(d) })

	leftOut := left.Map("tee.mutate", func(f Frame[*kv]) *kv {
		payload := f.Value()
		payload.Count = 99
		held := f.Get(teeKey)
		held.Count = 99
		held.Tags = append(held.Tags, "mutated")
		f.Set(teeKey, held)
		close(mutated)
		return payload
	}, WithReads[*kv](teeKey), WithWrites[*kv](teeKey)).Output("tee.left.out")

	rightOut := right.Map("tee.observe", func(f Frame[*kv]) *kv {
		<-mutated
		held := f.Get(teeKey)
		rightSeen <- observation{payloadCount: f.Value().Count, heldCount: held.Count, heldTags: len(held.Tags)}
		return f.Value()
	}, WithReads[*kv](teeKey)).Output("tee.right.out")

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, &kv{Key: "payload", Count: 1, Tags: []string{"original"}})

	leftFrame := drain(t, leftOut, 1)[0]
	rightFrame := drain(t, rightOut, 1)[0]
	upstream := <-ids
	o := <-rightSeen

	if o.payloadCount != 1 {
		t.Fatalf("the right branch saw payload count %d after the left mutated its own copy", o.payloadCount)
	}
	if o.heldCount != 1 || o.heldTags != 1 {
		t.Fatalf("the right branch saw held count %d and %d tags after the left mutated the pointee; "+
			"the frame clone is shallow", o.heldCount, o.heldTags)
	}
	if leftFrame.ID() == rightFrame.ID() {
		t.Fatal("both Tee branches carry one frame identity")
	}
	if leftFrame.ID() == upstream || rightFrame.ID() == upstream {
		t.Fatalf("a Tee branch kept the upstream identity %q; the split cloned once, not twice", upstream)
	}
	if leftFrame.Parent() != upstream {
		t.Fatalf("the left branch reports parent %q, want the upstream id %q", leftFrame.Parent(), upstream)
	}
	if rightFrame.Parent() != upstream {
		t.Fatalf("the right branch reports parent %q, want the upstream id %q", rightFrame.Parent(), upstream)
	}
}

func TestIfRoutesIntactFrame(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type lineage struct{ id, parent string }
	upstream := make(chan lineage, 1)

	m := New("if")
	src, ingest := m.Source[int]("if.source")
	watched := src.Map("if.watch", func(f Frame[int]) int {
		upstream <- lineage{id: f.ID(), parent: f.Parent()}
		return f.Value()
	})
	taken, skipped := watched.If("if.branch", func(f Frame[int]) bool { return f.Value() > 0 })
	out := taken.Output("if.taken.out")
	skipped.Drop("if.skipped.drop")

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 5)

	before := <-upstream
	got := drain(t, out, 1)[0]
	if got.ID() != before.id {
		t.Fatalf("If changed the frame identity from %q to %q; it must route the INTACT frame",
			before.id, got.ID())
	}
	if got.Parent() != before.parent {
		t.Fatalf("If reparented the frame from %q to %q; it must not reparent", before.parent, got.Parent())
	}
}

func TestFIFOProcessesSerially(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const data = 20
	var mutex sync.Mutex
	inFlight, peak := 0, 0

	m := New("fifo", OptionFIFO)
	src, ingest := m.Source[int]("fifo.source", WithEdge(Channel[int](64)))
	out := src.Map("fifo.node", func(f Frame[int]) int {
		mutex.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mutex.Unlock()
		time.Sleep(time.Millisecond)
		mutex.Lock()
		inFlight--
		mutex.Unlock()
		return f.Value()
	}, WithEdge(Channel[int](64))).Output("fifo.out", WithEdge(Channel[int](64)))

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, data, 1)
	drain(t, out, data)

	mutex.Lock()
	got := peak
	mutex.Unlock()
	if got != 1 {
		t.Fatalf("peak in-flight was %d under OptionFIFO, want 1; the option is not wired", got)
	}
}

func TestOptionStoreReplacesHeap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newCountingStore()
	m := New("store", OptionStore(store))
	written := make(chan struct{}, 1)
	src, ingest := m.Source[int]("store.source")
	src.Map("store.node", func(f Frame[int]) int {
		f.Save(injectedCell, f.Value())
		written <- struct{}{}
		return f.Value()
	}, WithWrites[int](injectedCell)).Drop("store.drop")

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 11)
	awaitSignal(t, "the node's heap write", written)

	saves, raw, ok := store.snapshot(injectedCell.Name())
	if saves == 0 {
		t.Fatal("the injected store saw no writes; OptionStore is inert")
	}
	if !ok {
		t.Fatal("the injected store holds no value under the declared cell")
	}
	if raw != 11 {
		t.Fatalf("the injected store holds %v, want 11", raw)
	}
	host, hostOK := m.Host().Load(injectedCell)
	if !hostOK || host != 11 {
		t.Fatalf("Host reads %d (present=%t) and so does not read through the injected store", host, hostOK)
	}
}

func TestChainCyclePanicAndState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const data = 4
	m := New("chain")
	src, ingest := m.Source[int]("chain.source", WithEdge(Channel[int](16)))

	seeded := src.Map("chain.seed", func(f Frame[int]) int {
		f.Set(chainFactorKey, 3)
		return f.Value()
	}, WithReads[int](chainFactorKey), WithWrites[int](chainFactorKey))

	recursed := seeded.Recurse("chain.recurse", func(f Frame[Monad[int]]) Monad[int] {
		next := f.Value()
		factor := f.Get(chainFactorKey)
		return func(inner Frame[int]) int {
			if inner.Value() > 50 {
				return inner.Value()
			}
			return next(rewrap(inner, inner.Value()*factor))
		}
	}, WithReads[int](chainFactorKey))

	memoized := recursed.Memoize("chain.memoize", func(f Frame[Monad[int]]) Monad[int] {
		next := f.Value()
		return func(inner Frame[int]) int {
			if inner.Value()%2 == 0 {
				return inner.Value()
			}
			return next(rewrap(inner, inner.Value()+1))
		}
	}, strconv.Itoa)

	kept, discarded := memoized.If("chain.if", func(f Frame[int]) bool { return f.Value() > 50 })
	discarded.Drop("chain.if.drop")

	left, right := kept.Tee("chain.tee", func(d int) (int, int) { return d, d })
	right.Drop("chain.tee.drop")

	out := left.Map("chain.count", func(f Frame[int]) string {
		f.Update(chainSeenCell, func(n int) int { return n + 1 })
		return strconv.Itoa(f.Value())
	}, WithReads[int](chainSeenCell), WithWrites[int](chainSeenCell)).Output("chain.out")

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, data, 1)

	for _, f := range drain(t, out, data) {
		if f.Value() != "82" {
			t.Fatalf("the chain computed %q, want %q", f.Value(), "82")
		}
		if f.Source() != "chain.source" {
			t.Fatalf("the frame source is %q after the traversal, want %q", f.Source(), "chain.source")
		}
	}

	seen, ok := m.Host().Load(chainSeenCell)
	if !ok {
		t.Fatal("the heap cell holds no value after the traversal")
	}
	if seen != data {
		t.Fatalf("the heap cell counted %d data, want %d", seen, data)
	}
}

func TestCycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := New("cycle")
	src, ingest := m.Source[int]("cycle.source")
	incremented := src.Map("cycle.increment", func(f Frame[int]) int { return f.Value() + 1 })
	loop, exit := incremented.If("cycle.gate", func(f Frame[int]) bool { return f.Value() < 5 })
	loop.Send(src)
	out := exit.Output("cycle.out")

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 0)

	got := drain(t, out, 1)[0]
	if got.Value() != 5 {
		t.Fatalf("the cycle produced %d, want 5", got.Value())
	}
}

func TestPanicRoutesToTypedHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errs := make(chan NodeError[int], 1)
	m := New("panic-typed")
	src, ingest := m.Source[int]("panic-typed.source")
	src.Map("panic-typed.node", func(Frame[int]) int {
		panic("node exploded")
	}, WithErrorHandler(func(e NodeError[int]) { errs <- e })).Drop("panic-typed.drop")

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 3)

	e := awaitError(t, errs)
	if !e.Panic {
		t.Fatal("a recovered panic was not marked as a panic")
	}
	if e.Node != "panic-typed.node" {
		t.Fatalf("the handler received node %q, want %q", e.Node, "panic-typed.node")
	}
	if e.Payload != 3 {
		t.Fatalf("the handler received payload %d, want 3", e.Payload)
	}
	if e.Err == nil || !strings.Contains(e.Err.Error(), "node exploded") {
		t.Fatalf("the handler received %v, want an error naming the panic value", e.Err)
	}
}

func TestGlobalErasedHandlerFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errs := make(chan NodeError[any], 1)
	m := New("panic-global", OptionErrorHandler(func(e NodeError[any]) { errs <- e }))
	src, ingest := m.Source[int]("panic-global.source")
	src.Map("panic-global.node", func(Frame[int]) int {
		panic("global path")
	}).Drop("panic-global.drop")

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 9)

	e := awaitError(t, errs)
	if e.Node != "panic-global.node" {
		t.Fatalf("the global handler received node %q, want %q", e.Node, "panic-global.node")
	}
	payload, ok := e.Payload.(int)
	if !ok || payload != 9 {
		t.Fatalf("the global handler received payload %v, want the int 9", e.Payload)
	}
}

func TestPanicReclaimsFrameState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	captured := make(chan Frame[int], 1)
	held := make(chan int, 1)
	p := newProbe()
	m := New("reclaim", p.options...)
	src, ingest := m.Source[int]("reclaim.source")
	src.Map("reclaim.node", func(f Frame[int]) int {
		f.Set(reclaimKey, "held")
		held <- len(f.Data().Values)
		captured <- f
		panic("after writing state")
	}, WithReads[int](reclaimKey), WithWrites[int](reclaimKey)).Drop("reclaim.drop")

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 1)

	var frame Frame[int]
	select {
	case frame = <-captured:
	case <-time.After(10 * time.Second):
		t.Fatal("the node never ran")
	}
	// The known positive: the frame really did carry state before the panic, so an
	// empty projection below cannot pass vacuously. It is measured INSIDE the node,
	// because a frame's state has a single owner and reading it from here while the
	// node still owns it would be a race rather than an observation.
	if before := <-held; before != 1 {
		t.Fatalf("the node's frame projected %d values before the panic, want 1", before)
	}
	// The span ENDS after the guard reclaims: guard dispatches, then releases the
	// frame state, and only then finishes, which is what calls span.End(). So
	// observing an ended span for the node is what orders this goroutine's read of
	// the frame after the reclaim.
	pollUntil(t, "the panicking node ended its span", func() bool {
		for _, name := range spanNames(p.spans.Ended()) {
			if name == "reclaim.node" {
				return true
			}
		}
		return false
	})
	if got := len(frame.Data().Values); got != 0 {
		t.Fatalf("the panicking node's frame still projects %d values; the guard dispatched without reclaiming",
			got)
	}
}

func TestDuplicateNodeNameFailsStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := New("duplicate")
	src, _ := m.Source[int]("duplicate.source")
	src.Map("duplicate.node", func(f Frame[int]) int { return f.Value() }).Drop("duplicate.node")

	err := m.Start(ctx)
	if err == nil {
		t.Fatal("Start accepted a duplicate node name")
	}
	if !strings.Contains(err.Error(), "duplicate node name") {
		t.Fatalf("Start returned %v, want an error naming the duplicate node name", err)
	}
}

func TestUnconsumedFlowFailsStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := New("unconsumed")
	src, _ := m.Source[int]("unconsumed.source")
	src.Map("unconsumed.node", func(f Frame[int]) int { return f.Value() })

	err := m.Start(ctx)
	if err == nil {
		t.Fatal("Start accepted a flow that is never consumed")
	}
	if !strings.Contains(err.Error(), "never consumed") {
		t.Fatalf("Start returned %v, want an error saying the flow is never consumed", err)
	}
	if !strings.Contains(err.Error(), "unconsumed.node") {
		t.Fatalf("Start returned %v, want an error naming the producing node", err)
	}
}

func TestMaxConcurrencyBoundsInFlight(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const limit = 3
	const data = 30
	var mutex sync.Mutex
	inFlight, peak := 0, 0

	m := New("bounded", OptionMaxConcurrency(limit))
	src, ingest := m.Source[int]("bounded.source", WithEdge(Channel[int](64)))
	out := src.Map("bounded.node", func(f Frame[int]) int {
		mutex.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mutex.Unlock()
		time.Sleep(5 * time.Millisecond)
		mutex.Lock()
		inFlight--
		mutex.Unlock()
		return f.Value()
	}, WithEdge(Channel[int](64))).Output("bounded.out", WithEdge(Channel[int](64)))

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, data, 1)
	drain(t, out, data)

	mutex.Lock()
	got := peak
	mutex.Unlock()
	if got > limit {
		t.Fatalf("peak in-flight was %d, want at most %d", got, limit)
	}
	if got < 2 {
		t.Fatalf("peak in-flight was %d, so the run never exercised concurrency at all", got)
	}
}

func TestMachineReportsItsName(t *testing.T) {
	if got := New("named-machine").Name(); got != "named-machine" {
		t.Fatalf("Name returned %q, want %q", got, "named-machine")
	}
}

func TestNewKeyRefusesANilCloner(t *testing.T) {
	assertPanics(t, "NewKey with a nil cloner", func() { NewKey[int]("gate/nil-cloner", nil) })
	// The same name declares cleanly once a cloner is supplied. That is the known
	// positive AND the proof that the cloner check runs BEFORE the name is reserved: a
	// refusal that reserved first would make this second call panic on the duplicate.
	if got := NewKey("gate/nil-cloner", identityInt).Name(); got != "gate/nil-cloner" {
		t.Fatalf("NewKey returned a key named %q, want %q", got, "gate/nil-cloner")
	}
}

func TestUnboundFrameRefusesGatedAccess(t *testing.T) {
	// The frame RebuildFrame hands back was never bound to a node, so it carries no
	// capability view at all — the state no frame inside a running machine can reach.
	rebuilt, err := RebuildFrame(FrameData{ID: "rebuilt", Source: "origin", Node: "origin"}, 1)
	if err != nil {
		t.Fatalf("RebuildFrame refused a projection naming no keys: %v", err)
	}

	recovered := recoverPanic(t, "reading a stack key through an unbound frame", func() {
		_ = rebuilt.Get(unboundKey)
	})
	failure, ok := recovered.(error)
	if !ok {
		t.Fatalf("the unbound read panicked with %v (%T), want an error", recovered, recovered)
	}
	var capErr *CapabilityError
	if !errors.As(failure, &capErr) {
		t.Fatalf("the unbound read panicked with %v, want a *CapabilityError", failure)
	}
	if capErr.Node != unboundNode {
		t.Fatalf("the CapabilityError names node %q, want %q", capErr.Node, unboundNode)
	}
	if capErr.Key != unboundKey.Name() || capErr.Access != accessRead {
		t.Fatalf("the CapabilityError reports key %q access %q", capErr.Key, capErr.Access)
	}

	// The known positive: attaching a view that declares the key makes the SAME call
	// succeed, so the refusal above is the missing view rather than an accessor that
	// refuses everything.
	rebuilt.caps = testCapabilities("origin", []KeyRef{unboundKey}, nil)
	if got := rebuilt.Get(unboundKey); got != "" {
		t.Fatalf("the rebuilt frame holds %q under a key it never carried, want the zero value", got)
	}
}

func TestAbsentHeapCellReadsAsMissing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type observation struct {
		absentValue, presentValue int
		absentOK, presentOK       bool
	}
	seen := make(chan observation, 1)

	m := New("absent-heap")
	m.Host().Save(presentCell, 5)

	src, ingest := m.Source[int]("absent-heap.source")
	src.Map("absent-heap.node", func(f Frame[int]) int {
		var o observation
		o.absentValue, o.absentOK = f.Load(absentCell)
		o.presentValue, o.presentOK = f.Load(presentCell)
		seen <- o
		return f.Value()
	}, WithReads[int](absentCell, presentCell)).Drop("absent-heap.drop")

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 1)

	o := await(t, "the node's heap reads", seen)
	// The written cell is the known positive on both accessors: the same call against the
	// same store reports presence, so each miss below is an absent cell rather than a
	// read that can only ever report absence.
	if !o.presentOK || o.presentValue != 5 {
		t.Fatalf("Frame.Load read %d (present=%t) from a written cell, want 5 and present",
			o.presentValue, o.presentOK)
	}
	if o.absentOK {
		t.Fatal("Frame.Load reported a never-written heap cell as present")
	}
	if o.absentValue != 0 {
		t.Fatalf("Frame.Load returned %d for a never-written heap cell, want the zero value", o.absentValue)
	}

	hostPresent, hostPresentOK := m.Host().Load(presentCell)
	if !hostPresentOK || hostPresent != 5 {
		t.Fatalf("Host.Load read %d (present=%t) from a written cell, want 5 and present",
			hostPresent, hostPresentOK)
	}
	hostAbsent, hostAbsentOK := m.Host().Load(absentCell)
	if hostAbsentOK {
		t.Fatal("Host.Load reported a never-written heap cell as present")
	}
	if hostAbsent != 0 {
		t.Fatalf("Host.Load returned %d for a never-written heap cell, want the zero value", hostAbsent)
	}
}

func TestMemoizeReturnsTheCachedResultForARepeatedIndex(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Fibonacci is the shape that reaches the cache HIT: computing fib(n-1) fills the
	// cache for every index below it, so the fib(n-2) that follows is served from the
	// cache rather than recomputed. A recursion that only ever descends never hits.
	const target = 10
	const distinctIndices = target + 1 // one per index from 10 down to 0
	const withoutMemoization = 177     // 2*fib(target+1)-1, the un-cached body count

	var calls atomic.Int64
	m := New("memoize")
	src, ingest := m.Source[int]("memoize.source", WithEdge(Channel[int](4)))
	out := src.Memoize("memoize.fib", func(f Frame[Monad[int]]) Monad[int] {
		next := f.Value()
		return func(inner Frame[int]) int {
			calls.Add(1)
			n := inner.Value()
			if n < 2 {
				return n
			}
			return next(rewrap(inner, n-1)) + next(rewrap(inner, n-2))
		}
	}, strconv.Itoa, WithEdge(Channel[int](4))).Output("memoize.out", WithEdge(Channel[int](4)))

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, target)

	got := drain(t, out, 1)[0]
	if got.Value() != 55 {
		t.Fatalf("the memoized recursion computed fib(%d) as %d, want 55", target, got.Value())
	}
	// The count is the assertion that the cache was READ, not merely written: the answer
	// alone is the same either way.
	if n := calls.Load(); n != distinctIndices {
		t.Fatalf("the recursion body ran %d times for fib(%d), want %d — one per distinct index; "+
			"%d is what the same recursion costs when no lookup ever hits",
			n, target, distinctIndices, withoutMemoization)
	}
}

func TestEmitFailureRoutesToTheNodesHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refused := errors.New("branch edge refused")
	errs := make(chan NodeError[int], 1)
	m := New("emit-failure")
	src, ingest := m.Source[int]("emit-failure.source")
	// The refusing edge is the INBOUND transport of the branch's terminal, so the failure
	// is raised on the If node's own emit path rather than inside its filter.
	taken, skipped := src.If("emit-failure.branch", func(Frame[int]) bool { return true },
		WithErrorHandler(func(e NodeError[int]) { errs <- e }))
	taken.Drop("emit-failure.dead", WithEdge[int](func(string) (Edge[int], error) {
		return &failingEdge[int]{ch: make(chan Frame[int]), err: refused}, nil
	}))
	skipped.Drop("emit-failure.skipped")

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 13)

	e := awaitError(t, errs)
	if e.Node != "emit-failure.branch" {
		t.Fatalf("the handler received node %q, want %q", e.Node, "emit-failure.branch")
	}
	if e.Panic {
		t.Fatal("the handler received Panic=true for a send failure")
	}
	if !errors.Is(e.Err, refused) {
		t.Fatalf("the handler received %v, want %v", e.Err, refused)
	}
	if e.Payload != 13 {
		t.Fatalf("the handler received payload %d, want 13", e.Payload)
	}
}

func TestReadLoopReturnsWhenItsInboundChannelCloses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Every other test cancels its machine on the way out, so wait for those loops to
	// unwind first: the zero this test ends on is only readable against a zero baseline.
	pollUntil(t, "every read loop from an earlier test unwound", func() bool {
		return readLoopGoroutines() == 0
	})

	inbound := &closableEdge[int]{ch: make(chan Frame[int], 1)}
	m := New("closed-inbound")
	// The source owns the ONLY read loop in this machine: a terminal declared with Output
	// has none, so the count below names one loop unambiguously.
	src, ingest := m.Source[int]("closed-inbound.source", WithEdge[int](func(string) (Edge[int], error) {
		return inbound, nil
	}))
	out := src.Output("closed-inbound.out", WithEdge(Channel[int](4)))

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 7)
	if got := drain(t, out, 1)[0]; got.Value() != 7 {
		t.Fatalf("the source forwarded %d, want 7", got.Value())
	}
	// The known positive: the probe SEES a running loop, so the zero after the close is a
	// loop that returned rather than a probe that never saw one.
	if running := readLoopGoroutines(); running < 1 {
		t.Fatal("no read loop is visible in a stack dump while the machine is processing")
	}

	if err := inbound.Close(); err != nil {
		t.Fatalf("closing the inbound edge: %v", err)
	}
	pollUntil(t, "the read loop returned after its inbound channel closed", func() bool {
		return readLoopGoroutines() == 0
	})
}
