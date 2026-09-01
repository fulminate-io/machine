// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ledger

import (
	"encoding/gob"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"

	"github.com/hashicorp/raft"
)

// Pin both external interfaces here so a library drift breaks this build rather
// than one node's behavior at run time.
var (
	_ raft.FSM                = (*fsm)(nil)
	_ raft.ConfigurationStore = (*fsm)(nil)
)

// fsm is the replicated state machine behind a Ledger: the journal's values, the
// index of the last entry it applied, and a broadcast channel readers park on
// while they wait for that index to catch up with a commit.
type fsm struct {
	mutex   sync.RWMutex
	values  map[string]Entry
	applied uint64
	wake    chan struct{}
	poison  error
	// configuration and configurationIndex track the last membership this state
	// machine applied. They live under the SAME mutex and are woken by the SAME
	// broadcast channel as the applied index, which is what lets a membership
	// reader park on progress and on a context at once without a second channel
	// being handed to raft.
	configuration      raft.Configuration
	configurationIndex uint64
}

func newFSM() *fsm {
	return &fsm{values: map[string]Entry{}, wake: make(chan struct{})}
}

// Apply commits one journal entry. It advances the tracked index on EVERY path,
// including the poisoned one: a reader waiting on a poisoned ledger must fail with
// the poison rather than hang forever on an index that will never arrive.
func (f *fsm) Apply(log *raft.Log) any {
	entry, err := DecodeEntry(log.Data)
	if err != nil {
		return f.poisonAt(log.Index, fmt.Errorf("ledger: log index %d: %w", log.Index, err))
	}

	return f.applyEntry(log.Index, entry)
}

// applyEntry runs the arm for a decoded entry's kind. The default arm catches a
// kind that was declared but never given an arm here, which DecodeEntry cannot
// see; it poisons rather than ignoring, on the same reasoning.
func (f *fsm) applyEntry(index uint64, entry Entry) any {
	switch entry.Kind {
	case KindSet:
		f.set(index, entry)

		return nil
	case KindEpoch:
		f.advance(index)

		return nil
	default:
		return f.poisonAt(index, fmt.Errorf(
			"ledger: log index %d applies kind %d, which this build declares but does not handle: %w",
			index, uint8(entry.Kind), ErrPoisonedJournal))
	}
}

// StoreConfiguration records a membership commit and advances the tracked index.
//
// IMPLEMENTING raft.ConfigurationStore IS NOT OPTIONAL HERE, and it is not only
// about membership. raft's own applySingle returns early for a configuration entry
// when the state machine does not implement this interface, deliberately not
// advancing its index either — so without this method a configuration commit
// would leave a reader parked behind an index the state machine never reaches.
//
// The membership and its index are recorded and the tracked index advanced in ONE
// critical section, for the reason set gives: no reader may observe the index
// without the value it accounts for.
func (f *fsm) StoreConfiguration(index uint64, configuration raft.Configuration) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.configuration = configuration
	f.configurationIndex = index
	f.advanceLocked(index)
}

// configurationView is the membership, the index it landed at, and the wake
// channel, read together under one lock.
//
// IT CARRIES NO POISON, DELIBERATELY. A poisoned journal is exactly when a
// recovery decision most needs to see membership, and this reports raft's OWN
// configuration rather than any journal value — so a poison that refuses journal
// reads must not also silence the signal a recovery path reads in order to act
// on it.
type configurationView struct {
	configuration raft.Configuration
	index         uint64
	wake          chan struct{}
}

// observeConfiguration reads the membership, its index and the wake channel as
// ONE consistent triple, the way observe does for the applied index.
func (f *fsm) observeConfiguration() configurationView {
	f.mutex.RLock()
	defer f.mutex.RUnlock()

	return configurationView{configuration: f.configuration, index: f.configurationIndex, wake: f.wake}
}

// set stores an entry and advances the index in ONE critical section, so no reader
// can observe the index without the value it accounts for.
func (f *fsm) set(index uint64, entry Entry) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.values[entry.Path] = entry
	f.advanceLocked(index)
}

// poisonAt records the first poison and advances past the entry that caused it.
func (f *fsm) poisonAt(index uint64, err error) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	if f.poison == nil {
		f.poison = err
	}
	f.advanceLocked(index)

	return err
}

// advance moves the tracked applied index and wakes everything parked below it.
func (f *fsm) advance(index uint64) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.advanceLocked(index)
}

// broadcast wakes everything parked on the tracker WITHOUT moving the index.
//
// It exists because a reader can be waiting on something that is not an index: a
// barrier parked until this term is established must re-evaluate when the epoch
// position is recorded, and that record is not an apply.
func (f *fsm) broadcast() {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	close(f.wake)
	f.wake = make(chan struct{})
}

// advanceLocked closes the current wake channel and installs a fresh one, which is
// what lets a waiter select on progress AND a context at the same time. A
// sync.Cond cannot be selected on, so a broadcast channel is the shape here.
func (f *fsm) advanceLocked(index uint64) {
	if index <= f.applied {
		return
	}

	f.applied = index
	close(f.wake)
	f.wake = make(chan struct{})
}

// get reads one journaled value.
func (f *fsm) get(path string) (Entry, bool) {
	f.mutex.RLock()
	defer f.mutex.RUnlock()

	entry, ok := f.values[path]

	return entry, ok
}

// fsmSnapshot is the point-in-time copy Persist serializes. Its fields are
// exported because gob is what writes them to the snapshot sink.
type fsmSnapshot struct {
	Values  map[string]Entry
	Applied uint64
}

var _ raft.FSMSnapshot = (*fsmSnapshot)(nil)

// Snapshot copies the journal under the read lock and returns immediately.
//
// It COPIES rather than aliasing the live map for two reasons that point the same
// way: raft states that Apply runs CONCURRENTLY with Persist, and an encoder
// reading a map another goroutine is writing is a concurrent read-write panic. This
// is the cataloged snapshot-map-before-serializing shape, and the copy is therefore
// not an optimization for a later reader to notice Persist "only reads" and remove.
//
// No I/O happens here. Apply cannot run while Snapshot does, so anything expensive
// belongs in Persist.
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	f.mutex.RLock()
	defer f.mutex.RUnlock()

	values := make(map[string]Entry, len(f.values))
	maps.Copy(values, f.values)

	return &fsmSnapshot{Values: values, Applied: f.applied}, nil
}

// Persist writes the copied journal to the sink, canceling the sink on failure so
// raft never keeps a half-written snapshot.
func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if err := gob.NewEncoder(sink).Encode(s); err != nil {
		_ = sink.Cancel()

		return fmt.Errorf("ledger: persisting a snapshot at applied index %d failed: %w", s.Applied, err)
	}

	return sink.Close()
}

// Release is called by raft unconditionally, including when Persist never ran, so
// it must be safe on a snapshot that was never persisted. This one holds only
// memory and has nothing to release.
func (*fsmSnapshot) Release() {}

// Restore replaces this state machine's entire contents with a snapshot's.
//
// It DISCARDS ALL PREVIOUS STATE before restoring, which raft's contract requires
// and which matters more here than the contract makes it sound: a Restore that
// merged the snapshot over the live map would leave two peers restoring the SAME
// snapshot from different priors in different states, which is the one thing a
// replicated state machine may never do.
//
// It also VALIDATES every kind it installs and CLEARS a poison recorded against the
// journal it just discarded — see replace, which owns that ordering. A rejoining
// peer therefore heals through an authoritative snapshot rather than staying
// read-dead on data that is correct.
func (f *fsm) Restore(snapshot io.ReadCloser) error {
	defer func() { _ = snapshot.Close() }()

	var restored fsmSnapshot
	if err := gob.NewDecoder(snapshot).Decode(&restored); err != nil {
		return fmt.Errorf("ledger: restoring a snapshot failed: %w", err)
	}
	f.replace(restored)

	return nil
}

// replace installs the restored journal wholesale and advances through the same
// tracker every other path uses, so a reader parked on a target below the
// snapshot's index wakes instead of being stranded by the restore.
//
// THE ORDER IS PRESCRIBED: clear the poison, then validate what is being installed,
// then record a fresh poison only if the RESTORED journal earns one.
//
// Clearing first is right because the poison tracks the state this node HOLDS, not
// what its build knows, and this restore has just discarded the journal that poison
// described — keeping it would leave a peer refusing correct authoritative data
// while naming a log index that no longer exists. Validating second is right because
// a kind arriving by snapshot is no more interpretable than the same kind arriving
// by log: if Apply poisons on it, so must this, or the fail-loud invariant would
// invert depending on which path an entry took to get here.
func (f *fsm) replace(restored fsmSnapshot) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.values = restored.Values
	if f.values == nil {
		f.values = map[string]Entry{}
	}
	f.poison = validateRestored(f.values)
	f.advanceLocked(restored.Applied)
}

// validateRestored reports the first restored entry whose kind this build cannot
// interpret, or nil when every kind is declared.
//
// Paths are walked in sorted order so the refusal names the same entry on every
// peer rather than whichever one map iteration happened to reach first.
func validateRestored(values map[string]Entry) error {
	paths := make([]string, 0, len(values))
	for path := range values {
		paths = append(paths, path)
	}
	slices.Sort(paths)

	for _, path := range paths {
		if entry := values[path]; !entry.Kind.declared() {
			return fmt.Errorf("ledger: restored entry for %q names kind %d, which this build does not declare: %w",
				path, uint8(entry.Kind), ErrPoisonedJournal)
		}
	}

	return nil
}
