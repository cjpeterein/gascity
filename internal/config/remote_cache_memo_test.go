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
