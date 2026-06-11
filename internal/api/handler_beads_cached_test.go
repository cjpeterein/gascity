package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// primedCacheOverStaleBacking builds a CachingStore primed before a
// backing-only mutation, so cached reads observe the pre-mutation state and
// live reads observe the post-mutation state. Returns the cache and the
// mutate callback to apply after priming.
func primedCacheOverStaleBacking(t *testing.T) (*beads.CachingStore, *beads.MemStore) {
	t.Helper()
	backing := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(backing, nil)
	return cache, backing
}

// TestBeadListInProgressCachedServesFromCache is the cached=true mirror of
// TestBeadListInProgressUsesLiveLookup: a backing-store status flip applied
// after the prime must NOT be visible, proving the read came from the
// supervisor cache rather than the backing store.
func TestBeadListInProgressCachedServesFromCache(t *testing.T) {
	state := newFakeState(t)
	cache, backing := primedCacheOverStaleBacking(t)
	work, err := backing.Create(beads.Bead{Title: "active work"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	state.stores["myrig"] = cache
	status := "in_progress"
	if err := backing.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest("GET", cityURL(state, "/beads?status=in_progress&cached=true"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("cached in_progress beads = %+v, want none (backing-only flip must stay invisible)", resp.Items)
	}
}

// TestBeadListInProgressCachedSeesCacheFedUpdates proves the cached read path
// still returns in_progress rows when the mutation went through the cache
// (the supervisor write path).
func TestBeadListInProgressCachedSeesCacheFedUpdates(t *testing.T) {
	state := newFakeState(t)
	cache, _ := primedCacheOverStaleBacking(t)
	work, err := cache.Create(beads.Bead{Title: "active work"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	state.stores["myrig"] = cache
	status := "in_progress"
	if err := cache.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest("GET", cityURL(state, "/beads?status=in_progress&cached=true"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Items []beads.Bead `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].ID != work.ID {
		t.Fatalf("cached in_progress beads = %+v, want only %s", resp.Items, work.ID)
	}
}

// TestBeadReadyCachedServesFromCache is the cached=true mirror of
// TestBeadReadyUsesLiveLookup: closing the blocker directly on the backing
// store after the prime must NOT unblock the dependent bead on the cached
// read path.
func TestBeadReadyCachedServesFromCache(t *testing.T) {
	state := newFakeState(t)
	cache, backing := primedCacheOverStaleBacking(t)
	blocker, err := backing.Create(beads.Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create(blocker): %v", err)
	}
	ready, err := backing.Create(beads.Bead{Title: "ready"})
	if err != nil {
		t.Fatalf("Create(ready): %v", err)
	}
	if err := backing.DepAdd(ready.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	state.stores["myrig"] = cache
	if err := backing.Close(blocker.ID); err != nil {
		t.Fatalf("Close(blocker): %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest("GET", cityURL(state, "/beads/ready?cached=true"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Items []beads.Bead `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// blocker is still open in the cache, so only the blocker itself is ready.
	if len(resp.Items) != 1 || resp.Items[0].ID != blocker.ID {
		t.Fatalf("cached ready beads = %+v, want only %s (dependent must stay blocked)", resp.Items, blocker.ID)
	}
}

// failingCachedReader is a CachedReader whose every read fails, simulating an
// unprimed or degraded supervisor cache.
type failingCachedReader struct{}

func (failingCachedReader) Get(string) (beads.Bead, error) {
	return beads.Bead{}, errors.Join(beads.ErrCacheUnavailable, errors.New("cache down"))
}

func (failingCachedReader) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.Join(beads.ErrCacheUnavailable, errors.New("cache down"))
}

func (failingCachedReader) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	return nil, errors.Join(beads.ErrCacheUnavailable, errors.New("cache down"))
}

func (failingCachedReader) DepList(string, string) ([]beads.Dep, error) {
	return nil, errors.Join(beads.ErrCacheUnavailable, errors.New("cache down"))
}

// degradedCacheStore exposes a failing Cached handle over a working live
// store so the fallback path is observable.
type degradedCacheStore struct {
	beads.Store
}

func (s degradedCacheStore) Handles() beads.StoreHandles {
	handles := beads.HandlesFor(s.Store)
	handles.Cached = failingCachedReader{}
	return handles
}

// TestBeadReadyCachedFallsBackToLive proves cached=true degrades to the live
// reader instead of failing when the cache cannot answer.
func TestBeadReadyCachedFallsBackToLive(t *testing.T) {
	state := newFakeState(t)
	backing := beads.NewMemStore()
	work, err := backing.Create(beads.Bead{Title: "ready work"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	state.stores["myrig"] = degradedCacheStore{Store: backing}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest("GET", cityURL(state, "/beads/ready?cached=true"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Items []beads.Bead `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].ID != work.ID {
		t.Fatalf("ready beads = %+v, want only %s via live fallback", resp.Items, work.ID)
	}
}
