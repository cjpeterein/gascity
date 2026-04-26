package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

// A pack-imported V2 agent carries BindingName. Order beads route with the
// bare agent name (gc.routed_to = "dog"), which is also what $GC_TEMPLATE
// resolves to for ready-queue matching. The doctor check must treat that
// bare name as a live route target, not a stale one.
func TestSessionModelDoctorAcceptsBareRouteForV2Agent(t *testing.T) {
	cityPath := t.TempDir()
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(cityPath, ".gc", "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   "task",
		Status: "open",
		Title:  "order run",
		Metadata: map[string]string{
			"gc.routed_to": "dog",
		},
	}); err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:        "dog",
			BindingName: "gastown",
		}},
	}
	check := &sessionModelDoctorCheck{
		cfg:      cfg,
		cityPath: cityPath,
		newStore: func(string) (beads.Store, error) { return store, nil },
	}

	result := check.Run(&doctor.CheckContext{CityPath: cityPath})
	if result == nil {
		t.Fatalf("nil result")
	}
	for _, d := range result.Details {
		if strings.Contains(d, "stale-routed-config") {
			t.Fatalf("unexpected stale-routed-config for bare V2 route: %q", d)
		}
	}
	if strings.Contains(result.Message, "stale-routed-config") {
		t.Fatalf("unexpected stale-routed-config in message: %q", result.Message)
	}
}

// Unknown bare route targets must still be flagged. This guards against the
// bare-name fix becoming a blanket suppression of real misrouting.
func TestSessionModelDoctorStillFlagsUnknownRoute(t *testing.T) {
	cityPath := t.TempDir()
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(cityPath, ".gc", "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   "task",
		Status: "open",
		Title:  "order run",
		Metadata: map[string]string{
			"gc.routed_to": "ghost",
		},
	}); err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:        "dog",
			BindingName: "gastown",
		}},
	}
	check := &sessionModelDoctorCheck{
		cfg:      cfg,
		cityPath: cityPath,
		newStore: func(string) (beads.Store, error) { return store, nil },
	}

	result := check.Run(&doctor.CheckContext{CityPath: cityPath})
	if result == nil {
		t.Fatalf("nil result")
	}
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "stale-routed-config") && strings.Contains(d, "ghost") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected stale-routed-config for unknown route, got details=%v message=%q", result.Details, result.Message)
	}
}

// Ambiguous bare routes (two agents share a Name across different bindings)
// cannot be resolved to a single agent — doctor must flag these so the
// operator can disambiguate before real routing breaks.
func TestSessionModelDoctorFlagsAmbiguousBareRoute(t *testing.T) {
	cityPath := t.TempDir()
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(cityPath, ".gc", "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   "task",
		Status: "open",
		Title:  "order run",
		Metadata: map[string]string{
			"gc.routed_to": "dog",
		},
	}); err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "dog", BindingName: "gastown"},
			{Name: "dog", BindingName: "kennel"},
		},
	}
	check := &sessionModelDoctorCheck{
		cfg:      cfg,
		cityPath: cityPath,
		newStore: func(string) (beads.Store, error) { return store, nil },
	}

	result := check.Run(&doctor.CheckContext{CityPath: cityPath})
	if result == nil {
		t.Fatalf("nil result")
	}
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "stale-routed-config") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected stale-routed-config for ambiguous bare route, got details=%v", result.Details)
	}
}
