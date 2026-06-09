package beads

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type reconcileRaceStore struct {
	Store
	started chan struct{}
	release chan struct{}
	stale   []Bead

	mu    sync.Mutex
	block bool
	once  sync.Once

	afterStaleDepListID string
	afterStaleDepList   func()
	depOnce             sync.Once
}

func (s *reconcileRaceStore) List(query ListQuery) ([]Bead, error) {
	if !query.AllowScan {
		return s.Store.List(query)
	}

	s.mu.Lock()
	block := s.block
	s.mu.Unlock()
	if !block {
		return s.Store.List(query)
	}

	s.once.Do(func() {
		close(s.started)
	})
	<-s.release
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Bead(nil), s.stale...), nil
}

func (s *reconcileRaceStore) DepList(id, direction string) ([]Dep, error) {
	deps, err := s.Store.DepList(id, direction)
	if err == nil && id == s.afterStaleDepListID && s.afterStaleDepList != nil {
		s.depOnce.Do(s.afterStaleDepList)
	}
	return deps, err
}

func TestCachingStoreReconciliationPreservesConcurrentMutation(t *testing.T) {
	mem := NewMemStore()
	original, err := mem.Create(Bead{Title: "before reconcile"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	backing := &reconcileRaceStore{
		Store:   mem,
		started: make(chan struct{}),
		release: make(chan struct{}),
		stale:   []Bead{original},
	}
	cs := NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.mu.Lock()
	backing.block = true
	backing.mu.Unlock()

	done := make(chan struct{})
	go func() {
		cs.runReconciliation()
		close(done)
	}()

	<-backing.started
	title := "after concurrent update"
	if err := cs.Update(original.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	close(backing.release)
	<-done

	items, err := cs.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(items) != 1 || items[0].Title != title {
		t.Fatalf("ListOpen = %#v, want updated title %q", items, title)
	}
}

func TestCachingStoreReconciliationPreservesConcurrentEvent(t *testing.T) {
	mem := NewMemStore()
	original, err := mem.Create(Bead{Title: "before reconcile"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	backing := &reconcileRaceStore{
		Store:   mem,
		started: make(chan struct{}),
		release: make(chan struct{}),
		stale:   []Bead{original},
	}
	cs := NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.mu.Lock()
	backing.block = true
	backing.mu.Unlock()

	done := make(chan struct{})
	go func() {
		cs.runReconciliation()
		close(done)
	}()

	<-backing.started
	eventBead := cloneBead(original)
	eventBead.Title = "after concurrent event"
	payload, err := json.Marshal(eventBead)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	cs.ApplyEvent("bead.updated", payload)
	close(backing.release)
	<-done

	items, err := cs.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(items) != 1 || items[0].Title != eventBead.Title {
		t.Fatalf("ListOpen = %#v, want event title %q", items, eventBead.Title)
	}
}

func TestCachingStoreReconciliationPreservesConcurrentDependencyInvalidation(t *testing.T) {
	mem := NewMemStore()
	blocker, err := mem.Create(Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create(blocker): %v", err)
	}
	target, err := mem.Create(Bead{Title: "target"})
	if err != nil {
		t.Fatalf("Create(target): %v", err)
	}

	backing := &reconcileRaceStore{Store: mem}
	cs := NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.afterStaleDepListID = target.ID
	backing.afterStaleDepList = func() {
		if err := mem.DepAdd(target.ID, blocker.ID, "blocks"); err != nil {
			t.Errorf("DepAdd: %v", err)
			return
		}
		payload, err := json.Marshal(target)
		if err != nil {
			t.Errorf("Marshal: %v", err)
			return
		}
		cs.ApplyEvent("bead.updated", payload)
	}

	cs.runReconciliation()

	if ready, ok := cs.CachedReady(); ok {
		t.Fatalf("CachedReady answered from stale dependency cache after concurrent invalidation: %v", ready)
	}
	ready, err := cs.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	for _, bead := range ready {
		if bead.ID == target.ID {
			t.Fatalf("Ready includes %s after backing dependency add; ready=%v", target.ID, ready)
		}
	}
}

func TestCachingStoreReconciliationSkipsReemitForAlreadyClosedBead(t *testing.T) {
	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "to be closed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var events []string
	cs := NewCachingStoreForTest(mem, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cs.Close(bead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wantClose := "bead.closed:" + bead.ID
	closeSeen := false
	for _, e := range events {
		if e == wantClose {
			closeSeen = true
			break
		}
	}
	if !closeSeen {
		t.Fatalf("events after Close = %v, want to include %q", events, wantClose)
	}
	events = nil

	cs.runReconciliation()

	for _, e := range events {
		if strings.HasPrefix(e, "bead.closed:") {
			t.Fatalf("reconciliation re-emitted close event: %v", events)
		}
	}

	cs.mu.RLock()
	_, stillCached := cs.beads[bead.ID]
	cs.mu.RUnlock()
	if stillCached {
		t.Fatalf("closed bead %s should be evicted from cache after reconcile", bead.ID)
	}
}

func TestCachingStoreReconciliationSkipsReemitForAlreadyClosedBeadWithConcurrentMutation(t *testing.T) {
	mem := NewMemStore()
	closedBead, err := mem.Create(Bead{Title: "closed before reconcile"})
	if err != nil {
		t.Fatalf("Create(closed): %v", err)
	}
	other, err := mem.Create(Bead{Title: "concurrent target"})
	if err != nil {
		t.Fatalf("Create(other): %v", err)
	}

	backing := &reconcileRaceStore{
		Store:   mem,
		started: make(chan struct{}),
		release: make(chan struct{}),
		stale:   []Bead{other},
	}

	var events []string
	var eventsMu sync.Mutex
	cs := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, eventType+":"+beadID)
	})
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cs.Close(closedBead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	eventsMu.Lock()
	events = nil
	eventsMu.Unlock()

	backing.mu.Lock()
	backing.block = true
	backing.mu.Unlock()

	done := make(chan struct{})
	go func() {
		cs.runReconciliation()
		close(done)
	}()

	<-backing.started
	title := "after concurrent update"
	if err := cs.Update(other.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update(other): %v", err)
	}
	close(backing.release)
	<-done

	eventsMu.Lock()
	defer eventsMu.Unlock()
	for _, e := range events {
		if strings.HasPrefix(e, "bead.closed:") {
			t.Fatalf("reconciliation re-emitted close event in race path: %v", events)
		}
	}

	cs.mu.RLock()
	_, stillCached := cs.beads[closedBead.ID]
	cs.mu.RUnlock()
	if stillCached {
		t.Fatalf("closed bead %s should be evicted from cache after reconcile", closedBead.ID)
	}
}

func TestCachingStoreReconciliationMergesFreshDataWithConcurrentMutation(t *testing.T) {
	mem := NewMemStore()
	mutated, err := mem.Create(Bead{Title: "before mutate"})
	if err != nil {
		t.Fatalf("Create(mutated): %v", err)
	}
	refreshed, err := mem.Create(Bead{Title: "before refresh"})
	if err != nil {
		t.Fatalf("Create(refreshed): %v", err)
	}

	backing := &reconcileRaceStore{
		Store:   mem,
		started: make(chan struct{}),
		release: make(chan struct{}),
		stale:   []Bead{mutated, refreshed},
	}
	cs := NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.mu.Lock()
	backing.block = true
	backing.mu.Unlock()

	done := make(chan struct{})
	go func() {
		cs.runReconciliation()
		close(done)
	}()

	<-backing.started
	title := "after concurrent update"
	if err := cs.Update(mutated.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update(mutated): %v", err)
	}
	refreshedTitle := "after reconcile refresh"
	if err := mem.Update(refreshed.ID, UpdateOpts{Title: &refreshedTitle}); err != nil {
		t.Fatalf("Update(refreshed backing): %v", err)
	}
	refreshedBead, err := mem.Get(refreshed.ID)
	if err != nil {
		t.Fatalf("Get(refreshed backing): %v", err)
	}
	backing.mu.Lock()
	backing.stale = []Bead{
		cloneBead(mutated),
		cloneBead(refreshedBead),
	}
	backing.mu.Unlock()
	close(backing.release)
	<-done

	items, err := cs.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	gotTitles := map[string]string{}
	for _, item := range items {
		gotTitles[item.ID] = item.Title
	}
	if gotTitles[mutated.ID] != title {
		t.Fatalf("mutated title = %q, want %q", gotTitles[mutated.ID], title)
	}
	if gotTitles[refreshed.ID] != refreshedTitle {
		t.Fatalf("refreshed title = %q, want %q", gotTitles[refreshed.ID], refreshedTitle)
	}
}

// TestRunReconciliationLogsSuccess asserts the per-reconcile success log
// line surfaces a heartbeat after the cache refreshes. Before this line
// existed, a reconciler running silently on stale data produced no
// operator-visible signal — the T7920 incident 2026-05-26 went undetected
// for 2h 31m.
func TestRunReconciliationLogsSuccess(t *testing.T) {
	logBuf := captureLog(t)

	mem := NewMemStore()
	if _, err := mem.Create(Bead{Title: "heartbeat target"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cs := NewCachingStoreForTestWithPrefix(mem, "test-rig", nil)
	if err := cs.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	cs.runReconciliation()

	out := logBuf.String()
	if !strings.Contains(out, "beads cache: reconciled") {
		t.Fatalf("expected reconcile success line, got:\n%s", out)
	}
	if !strings.Contains(out, "rig=test-rig") {
		t.Errorf("missing rig identity in log; out=%q", out)
	}
	for _, want := range []string{"beads=", "adds=", "updates=", "removes=", "took=", "cadence="} {
		if !strings.Contains(out, want) {
			t.Errorf("missing field %q in log; out=%q", want, out)
		}
	}
}

// TestRunReconciliationLogRateLimited asserts the success log line is
// rate-limited to cacheReconcileSuccessLogWindow (one minute). Two
// back-to-back reconciles emit exactly one line.
func TestRunReconciliationLogRateLimited(t *testing.T) {
	logBuf := captureLog(t)

	mem := NewMemStore()
	cs := NewCachingStoreForTestWithPrefix(mem, "test-rig", nil)
	if err := cs.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	cs.runReconciliation()
	cs.runReconciliation()
	cs.runReconciliation()

	out := logBuf.String()
	count := strings.Count(out, "beads cache: reconciled")
	if count != 1 {
		t.Errorf("expected 1 reconciled line within rate-limit window, got %d:\n%s", count, out)
	}
}

// TestRunReconciliationLogEmitsAgainAfterWindow asserts the success log
// line is re-emitted once the rate-limit window has elapsed. The test
// reaches into lastReconcileLogAt to advance the simulated clock without
// sleeping a real minute.
func TestRunReconciliationLogEmitsAgainAfterWindow(t *testing.T) {
	logBuf := captureLog(t)

	mem := NewMemStore()
	cs := NewCachingStoreForTestWithPrefix(mem, "test-rig", nil)
	if err := cs.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	cs.runReconciliation()

	// Backdate the rate-limit gate beyond the window so the next emit fires.
	cs.mu.Lock()
	cs.lastReconcileLogAt = cs.lastReconcileLogAt.Add(-2 * cacheReconcileSuccessLogWindow)
	cs.mu.Unlock()

	cs.runReconciliation()

	out := logBuf.String()
	count := strings.Count(out, "beads cache: reconciled")
	if count != 2 {
		t.Errorf("expected 2 reconciled lines after window elapsed, got %d:\n%s", count, out)
	}
}

// TestCachingStoreReconciliationDoesNotResurrectClosedBeadFromStaleList
// reproduces gc-62iua: the refinery closes a work bead in another process;
// the controller's cache learns of the close via ApplyEvent("bead.closed")
// and stamps the close-time updated_at. ~20s later runReconciliation runs a
// full backing scan that returns a STALE snapshot still showing the bead
// open (an older Dolt revision / JSONL mirror). The diff must not overwrite
// the fresher cached closed row with the stale open row, and must not emit a
// spurious bead.updated event reopening it. updated_at is the discriminator:
// the stale row carries an older timestamp than the cached close.
func TestCachingStoreReconciliationDoesNotResurrectClosedBeadFromStaleList(t *testing.T) {
	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "merged work"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	staleOpen := cloneBead(bead) // open snapshot, original (older) updated_at

	backing := &reconcileRaceStore{
		Store:   mem,
		started: make(chan struct{}),
		release: make(chan struct{}),
		stale:   []Bead{staleOpen},
	}

	var events []string
	var eventsMu sync.Mutex
	cs := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, eventType+":"+beadID)
	})
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// The refinery closes the bead in the backing store, stamping a newer
	// updated_at, then the close event reaches this process's cache.
	if err := mem.Close(bead.ID); err != nil {
		t.Fatalf("Close backing: %v", err)
	}
	closed, err := mem.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get closed: %v", err)
	}
	if !closed.UpdatedAt.After(staleOpen.UpdatedAt) {
		t.Fatalf("close did not advance updated_at: close=%s stale=%s", closed.UpdatedAt, staleOpen.UpdatedAt)
	}
	closePayload, err := json.Marshal(closed)
	if err != nil {
		t.Fatalf("Marshal close: %v", err)
	}
	cs.ApplyEvent("bead.closed", closePayload)
	eventsMu.Lock()
	events = nil
	eventsMu.Unlock()

	// Now reconcile against the stale OPEN snapshot.
	backing.mu.Lock()
	backing.block = true
	backing.mu.Unlock()
	done := make(chan struct{})
	go func() {
		cs.runReconciliation()
		close(done)
	}()
	<-backing.started
	close(backing.release)
	<-done

	got, err := cs.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("bead resurrected: status = %q, want closed", got.Status)
	}

	eventsMu.Lock()
	defer eventsMu.Unlock()
	for _, e := range events {
		if e == "bead.updated:"+bead.ID || e == "bead.created:"+bead.ID {
			t.Fatalf("reconciliation re-emitted a reopen event: %v", events)
		}
	}
}

// TestCachingStoreReconciliationPreservesRoutedToFromStaleList reproduces
// gc-gi24h: gc sling stamps gc.routed_to on an unclaimed open work bead in
// another process. This process's controller learns the routed value via a
// REMOTE bead.updated event (not a local write, so the recent-local-mutation
// guard does not protect it). A later runReconciliation full scan returns a
// STALE pre-sling snapshot — a lagging JSONL mirror or an older Dolt revision
// — carrying an older updated_at and no gc.routed_to. Without
// reconcileBackingStale the diff overwrites the fresher cached routed row with
// the stale one, silently clearing gc.routed_to: the bead then shows as ready
// work with empty routing metadata, so the controller's scale_check sees no
// routed-but-unassigned demand and never re-arms the pool — the bug's observed
// symptom (workless work hiding as ready work). updated_at is the
// discriminator: the slung row's timestamp is strictly newer than the stale
// scan row, so the cached row wins. A claimed bead retains its metadata for the
// same reason its assignment is a local mutation; only the unclaimed,
// remote-event-learned row is exposed to this race.
func TestCachingStoreReconciliationPreservesRoutedToFromStaleList(t *testing.T) {
	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "route me"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stalePreSling := cloneBead(bead) // no routed_to, original (older) updated_at

	backing := &reconcileRaceStore{
		Store:   mem,
		started: make(chan struct{}),
		release: make(chan struct{}),
		stale:   []Bead{stalePreSling},
	}
	cs := NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// Another process (gc sling) stamps gc.routed_to in the backing, advancing
	// updated_at. This process does not see it as a local write.
	if err := mem.SetMetadata(bead.ID, "gc.routed_to", "rig/polecat"); err != nil {
		t.Fatalf("backing SetMetadata: %v", err)
	}
	routed, err := mem.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get routed: %v", err)
	}
	if !routed.UpdatedAt.After(stalePreSling.UpdatedAt) {
		t.Fatalf("sling did not advance updated_at: routed=%s stale=%s", routed.UpdatedAt, stalePreSling.UpdatedAt)
	}
	// The bd on_update hook forwards the full issue JSON, which carries
	// updated_at, wrapped in a {"bead": ...} envelope.
	payload, err := json.Marshal(map[string]any{"bead": routed})
	if err != nil {
		t.Fatalf("Marshal event: %v", err)
	}
	cs.ApplyEvent("bead.updated", payload)

	if got, err := cs.Get(bead.ID); err != nil {
		t.Fatalf("Get after event: %v", err)
	} else if got.Metadata["gc.routed_to"] != "rig/polecat" {
		t.Fatalf("precondition: cache missing routed_to after remote event: %v", got.Metadata)
	}

	// Reconcile against the stale pre-sling snapshot.
	backing.mu.Lock()
	backing.block = true
	backing.mu.Unlock()
	done := make(chan struct{})
	go func() {
		cs.runReconciliation()
		close(done)
	}()
	<-backing.started
	close(backing.release)
	<-done

	got, err := cs.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if got.Metadata["gc.routed_to"] != "rig/polecat" {
		t.Fatalf("gc.routed_to cleared by stale reconcile: metadata=%v", got.Metadata)
	}
}

// TestCachingStoreClosedEventStampsUpdatedAt reproduces the second half of
// gc-62iua: a rich bead.closed event carrying a fresh updated_at must update
// the cached row's UpdatedAt so the cache's own timestamp is authoritative.
// Without this, the cached closed row keeps its pre-close timestamp and a
// later stale reconcile scan cannot be distinguished from the close.
func TestCachingStoreClosedEventStampsUpdatedAt(t *testing.T) {
	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "stamp me"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cs := NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := mem.Close(bead.ID); err != nil {
		t.Fatalf("Close backing: %v", err)
	}
	closed, err := mem.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get closed: %v", err)
	}
	if !closed.UpdatedAt.After(bead.UpdatedAt) {
		t.Fatalf("close did not advance updated_at: close=%s create=%s", closed.UpdatedAt, bead.UpdatedAt)
	}
	closePayload, err := json.Marshal(closed)
	if err != nil {
		t.Fatalf("Marshal close: %v", err)
	}
	cs.ApplyEvent("bead.closed", closePayload)

	cs.mu.RLock()
	cached := cs.beads[bead.ID]
	cs.mu.RUnlock()
	if cached.Status != "closed" {
		t.Fatalf("cached status = %q, want closed", cached.Status)
	}
	if !cached.UpdatedAt.Equal(closed.UpdatedAt) {
		t.Fatalf("cached UpdatedAt = %s, want close-time %s", cached.UpdatedAt, closed.UpdatedAt)
	}
}

// failingScanStore fails full-scan List calls (the Prime path) while
// letting status-filtered List calls (the PrimeActive path) through, so
// tests can model a store whose initial full prime fails.
type failingScanStore struct {
	Store

	mu       sync.Mutex
	failScan bool
}

func (s *failingScanStore) setFailScan(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failScan = fail
}

func (s *failingScanStore) List(query ListQuery) ([]Bead, error) {
	if query.AllowScan {
		s.mu.Lock()
		fail := s.failScan
		s.mu.Unlock()
		if fail {
			return nil, errors.New("full scan unavailable")
		}
	}
	return s.Store.List(query)
}

// TestRunReconciliationPromotesPartialCacheToLive asserts that a clean
// full-scan reconciliation promotes a PrimeActive-only (cachePartial)
// cache to live. A reconcile loads the same complete active snapshot a
// successful Prime would, so a store whose initial full prime failed must
// converge to live through reconciliation instead of serving its
// PrimeActive-era snapshot indefinitely.
func TestRunReconciliationPromotesPartialCacheToLive(t *testing.T) {
	mem := NewMemStore()
	primed, err := mem.Create(Bead{Title: "present at prime-active"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cs := NewCachingStoreForTest(mem, nil)
	if err := cs.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	if cs.IsLive() {
		t.Fatal("cache live after PrimeActive alone, want partial")
	}

	// A bead created behind the cache's back (no event delivered) models
	// storage-level state the partial snapshot missed.
	missed, err := mem.Create(Bead{Title: "missed by prime-active"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cs.runReconciliation()

	if !cs.IsLive() {
		t.Fatal("cache not live after clean reconcile, want promoted to live")
	}
	got, ok := cs.CachedReady()
	if !ok {
		t.Fatal("CachedReady not servable after reconcile promotion")
	}
	ids := make(map[string]bool, len(got))
	for _, b := range got {
		ids[b.ID] = true
	}
	if !ids[primed.ID] || !ids[missed.ID] {
		t.Fatalf("CachedReady = %v, want both %s and %s", ids, primed.ID, missed.ID)
	}
}

// TestRunReconciliationPromotesUnprimedCacheToLive asserts reconciliation
// also converges a cache whose PrimeActive never succeeded
// (cacheUninitialized), mirroring Prime's unconditional promotion.
func TestRunReconciliationPromotesUnprimedCacheToLive(t *testing.T) {
	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "storage-level work"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cs := NewCachingStoreForTest(mem, nil)

	cs.runReconciliation()

	if !cs.IsLive() {
		t.Fatal("cache not live after clean reconcile from uninitialized state")
	}
	got, ok := cs.CachedReady()
	if !ok {
		t.Fatal("CachedReady not servable after reconcile promotion")
	}
	if len(got) != 1 || got[0].ID != bead.ID {
		t.Fatalf("CachedReady = %#v, want only %s", got, bead.ID)
	}
}

// TestRunReconciliationDoesNotPromoteOnFailure asserts a failed reconcile
// leaves a partial cache partial — promotion requires a clean full scan.
func TestRunReconciliationDoesNotPromoteOnFailure(t *testing.T) {
	mem := NewMemStore()
	if _, err := mem.Create(Bead{Title: "present at prime-active"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &failingScanStore{Store: mem}
	cs := NewCachingStoreForTest(backing, nil)
	if err := cs.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	backing.setFailScan(true)

	cs.runReconciliation()

	if cs.IsLive() {
		t.Fatal("cache promoted to live by a FAILED reconcile")
	}
}

// TestPrimeFailureThenReconcileConverges is the end-to-end shape of the
// recovery path: PrimeActive succeeds, the full Prime fails, and a later
// clean reconciliation converges the cache to storage and promotes it
// live so cached readers stop falling back.
func TestPrimeFailureThenReconcileConverges(t *testing.T) {
	mem := NewMemStore()
	if _, err := mem.Create(Bead{Title: "present at prime-active"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &failingScanStore{Store: mem, failScan: true}
	cs := NewCachingStoreForTest(backing, nil)
	cs.primeRetryDelay = func(int) time.Duration { return 0 }
	if err := cs.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	if err := cs.Prime(context.Background()); err == nil {
		t.Fatal("Prime succeeded against failing scan store, want error")
	}

	missed, err := mem.Create(Bead{Title: "created while prime was failing"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	backing.setFailScan(false)
	cs.runReconciliation()

	if !cs.IsLive() {
		t.Fatal("cache not live after reconcile recovered from failed prime")
	}
	got, err := cs.cachedReadyOnly(ReadyQuery{TierMode: TierBoth})
	if err != nil {
		t.Fatalf("cachedReadyOnly: %v", err)
	}
	found := false
	for _, b := range got {
		if b.ID == missed.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("cachedReadyOnly = %#v, want to include %s", got, missed.ID)
	}
}
