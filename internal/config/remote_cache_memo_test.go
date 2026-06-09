package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRemoteCacheValidationMemo_WarmAfterStatus is a regression test for the
// git-status fork storm (gc-0kc). The first validation runs `git status
// --porcelain --ignored`, whose own index.lock create/unlink bumps the mtime of
// the cache's `.git` directory. If the fingerprint stats that directory, the
// memo self-invalidates and every subsequent config load re-execs git. The
// fingerprint must key off stable signals (checkout root + `.git/index` +
// `.git/HEAD`) so a warm, unchanged cache is validated exactly once per process.
func TestRemoteCacheValidationMemo_WarmAfterStatus(t *testing.T) {
	ResetRemoteCacheValidationCache()
	t.Cleanup(ResetRemoteCacheValidationCache)

	dir := t.TempDir()
	cacheRoot := filepath.Join(dir, "repos")
	cacheDir := filepath.Join(cacheRoot, "pack")
	mustMkdirAll(t, cacheDir, 0o755)
	writeTestFile(t, cacheDir, "pack.toml", "[pack]\nname = \"memo\"\nschema = 1\n")

	if _, err := runRepoCacheGit(cacheDir, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if _, err := runRepoCacheGit(cacheDir, "add", "pack.toml"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := runRepoCacheGit(cacheDir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "seed"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	commit, err := runRepoCacheGit(cacheDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}

	source := "https://github.com/example/memo.git"
	prev := runRepoCacheGit
	var statusExecs int
	runRepoCacheGit = func(d string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "status" && args[1] == "--porcelain" {
			statusExecs++
		}
		return prev(d, args...)
	}
	t.Cleanup(func() { runRepoCacheGit = prev })

	// First validation: cold memo, must run git (rev-parse + status). The status
	// run bumps the `.git` directory mtime.
	if err := validateInstalledRemoteCacheLocked(source, cacheRoot, cacheDir, commit); err != nil {
		t.Fatalf("first validation: %v", err)
	}
	if statusExecs != 1 {
		t.Fatalf("first validation: got %d status execs, want 1", statusExecs)
	}

	// Second validation: cache is unchanged, so the memo must hit and skip git
	// entirely. A fingerprint that stats the volatile `.git` directory would
	// see the bumped mtime, miss the memo, and re-exec git here.
	if err := validateInstalledRemoteCacheLocked(source, cacheRoot, cacheDir, commit); err != nil {
		t.Fatalf("second validation: %v", err)
	}
	if statusExecs != 1 {
		t.Fatalf("warm cache re-execed git status: got %d status execs across two validations, want 1", statusExecs)
	}

	// A `gc import install` re-checkout rewrites `.git/HEAD`. The memo must
	// notice and revalidate so a repaired or moved cache is picked up.
	headPath := filepath.Join(cacheDir, ".git", "HEAD")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(headPath, future, future); err != nil {
		t.Fatalf("touch HEAD: %v", err)
	}
	if err := validateInstalledRemoteCacheLocked(source, cacheRoot, cacheDir, commit); err != nil {
		t.Fatalf("third validation: %v", err)
	}
	if statusExecs != 2 {
		t.Fatalf("changed cache skipped revalidation: got %d status execs, want 2", statusExecs)
	}
}

// TestRemoteImportResolutionMemo_CollapsesPasses is a regression test for the
// redundant-resolution multiplier (gc-oae). Config load resolves the same
// remote import 18-27x per gc invocation (city graph, per-rig graphs,
// default-rig graph, revision hash, provenance snapshot, each over several
// LoadWithIncludes passes). The validation memo (gc-0kc) collapses the git
// execs, but each resolution still re-reads packs.lock, re-fingerprints, and
// re-enters validation. resolveLockedRemoteImport must memoize the resolved
// cacheDir per (source, cityRoot, packs.lock fingerprint) so each distinct
// import resolves O(1) times — and so a lockfile rewrite still forces a fresh
// validation.
func TestRemoteImportResolutionMemo_CollapsesPasses(t *testing.T) {
	ResetRemoteCacheValidationCache()
	t.Cleanup(ResetRemoteCacheValidationCache)

	home := t.TempDir()
	t.Setenv("HOME", home)
	cityRoot := t.TempDir()
	source := "https://github.com/example/resolve-memo.git"

	// Build the cache checkout in a scratch dir first: its commit hash is not
	// predictable, but the cacheDir path is derived from it via RepoCacheKey.
	scratch := filepath.Join(t.TempDir(), "checkout")
	mustMkdirAll(t, scratch, 0o755)
	writeTestFile(t, scratch, "pack.toml", "[pack]\nname = \"resolve-memo\"\nschema = 1\n")
	if _, err := runRepoCacheGit(scratch, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if _, err := runRepoCacheGit(scratch, "add", "pack.toml"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := runRepoCacheGit(scratch, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "seed"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	commit, err := runRepoCacheGit(scratch, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}

	cacheRoot := filepath.Join(home, ".gc", "cache", "repos")
	cacheDir := filepath.Join(cacheRoot, RepoCacheKey(source, commit))
	mustMkdirAll(t, cacheRoot, 0o755)
	if err := os.Rename(scratch, cacheDir); err != nil {
		t.Fatalf("relocate checkout: %v", err)
	}

	lockPath := filepath.Join(cityRoot, "packs.lock")
	writeTestFile(t, cityRoot, "packs.lock", "schema = 1\n\n[packs.\""+source+"\"]\ncommit = \""+commit+"\"\n")

	// Repeated resolutions of a clean cache all succeed and agree on the dir.
	for i := 0; i < 5; i++ {
		dir, ok, rErr := resolveLockedRemoteImport(source, cityRoot)
		if rErr != nil {
			t.Fatalf("resolve %d: %v", i, rErr)
		}
		if !ok || dir != cacheDir {
			t.Fatalf("resolve %d: got (%q, %v), want (%q, true)", i, dir, ok, cacheDir)
		}
	}

	// Corrupt the checkout WITHOUT touching packs.lock: an untracked file makes
	// `git status --ignored` non-empty and bumps the gc-0kc checkout
	// fingerprint, so an un-memoized resolution would re-validate and error.
	// The resolution memo is keyed on the packs.lock fingerprint, which is
	// unchanged, so it must still serve the warm cacheDir.
	writeTestFile(t, cacheDir, "stray.txt", "dirty\n")
	dir, ok, rErr := resolveLockedRemoteImport(source, cityRoot)
	if rErr != nil {
		t.Fatalf("warm resolve after checkout corruption re-validated: %v", rErr)
	}
	if !ok || dir != cacheDir {
		t.Fatalf("warm resolve after corruption: got (%q, %v), want (%q, true)", dir, ok, cacheDir)
	}

	// Rewriting packs.lock (an `gc import install`/upgrade) bumps its
	// fingerprint, so the memo must invalidate and re-validate — which now
	// catches the dirty checkout and errors.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(lockPath, future, future); err != nil {
		t.Fatalf("touch packs.lock: %v", err)
	}
	if _, _, rErr := resolveLockedRemoteImport(source, cityRoot); rErr == nil {
		t.Fatal("packs.lock change did not force revalidation: dirty checkout resolved clean")
	}
}
