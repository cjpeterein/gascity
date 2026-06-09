package config

import (
	"os"
	"testing"
)

// TestMain scrubs the machine-local cache-root env overrides that the running
// shell may export (gc prime sets GC_HOME for agent sessions). Tests in this
// package isolate the repo cache via t.Setenv("HOME", t.TempDir()); an
// inherited GC_HOME or GC_REPO_CACHE_ROOT would otherwise win in RepoCacheRoot
// and route reads/writes into the live ~/.gc/cache/repos, stomping the shared
// synthetic-pack marker and bricking the running city (gc-c7l). Tests that
// assert override behavior re-set the relevant key with t.Setenv, which is
// per-test scoped.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("GC_HOME")
	_ = os.Unsetenv("GC_REPO_CACHE_ROOT")
	os.Exit(m.Run())
}
