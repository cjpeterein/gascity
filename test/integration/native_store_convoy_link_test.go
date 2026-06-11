//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/doctor"
)

// TestNativeStoreConvoyTrackItemAgainstBDSchema reproduces gc-2x0: the sling
// auto-convoy link (convoy.TrackItem -> NativeDoltStore.DepAdd) must succeed
// against a database whose schema was created by the real bd binary.
//
// bd's deterministic-dependency-id migration (gastownhall/beads#4259) made
// dependencies.id a NOT NULL primary key with no DB-side default, so every
// insert path must supply the id client-side. A gc binary linking an older
// beads library inserts without the id and fails with "Field 'id' doesn't
// have a default value" — the exact failure this test pins down.
func TestNativeStoreConvoyTrackItemAgainstBDSchema(t *testing.T) {
	requireDoltIntegration(t)
	env := newIsolatedToolEnv(t, true)

	rootDir := t.TempDir()
	doltDataDir := filepath.Join(rootDir, "dolt")
	wsDir := filepath.Join(rootDir, "ws")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("creating workspace: %v", err)
	}
	gitCmd := exec.Command("git", "init", "--quiet")
	gitCmd.Dir = wsDir
	if out, err := gitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	serverPort := startSharedDoltServer(t, env, doltDataDir)
	runBDInit(t, env, wsDir, "cv", serverPort)
	configureCustomTypes(t, env, wsDir, doctor.RequiredCustomTypes)

	store, err := beads.OpenNativeDoltStoreAt(context.Background(), wsDir, nil)
	if err != nil {
		t.Fatalf("opening native store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.CloseStore(); err != nil {
			t.Errorf("closing native store: %v", err)
		}
	})

	item, err := store.Create(beads.Bead{Title: "tracked work item"})
	if err != nil {
		t.Fatalf("Create item: %v", err)
	}
	convoyBead, err := store.Create(beads.Bead{
		Title: "sling-" + item.ID,
		Type:  "convoy",
	})
	if err != nil {
		t.Fatalf("Create convoy: %v", err)
	}

	if err := convoy.TrackItem(store, convoyBead.ID, item.ID); err != nil {
		t.Fatalf("TrackItem against bd-created schema: %v", err)
	}

	deps, err := store.DepList(convoyBead.ID, "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	var found bool
	for _, dep := range deps {
		if dep.IssueID == convoyBead.ID && dep.DependsOnID == item.ID && dep.Type == convoy.TrackingDepType {
			found = true
		}
	}
	if !found {
		t.Fatalf("tracks dependency %s -> %s not recorded; deps=%v", convoyBead.ID, item.ID, deps)
	}
}
