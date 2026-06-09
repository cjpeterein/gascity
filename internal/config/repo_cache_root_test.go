package config

import (
	"path/filepath"
	"testing"
)

func TestRepoCacheRootDefaultsToHomeGC(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", "")
	t.Setenv(RepoCacheRootEnv, "")

	got, err := RepoCacheRoot()
	if err != nil {
		t.Fatalf("RepoCacheRoot: %v", err)
	}
	want := filepath.Join(home, ".gc", "cache", "repos")
	if got != want {
		t.Fatalf("RepoCacheRoot = %q, want %q", got, want)
	}
}

func TestRepoCacheRootHonorsGCHome(t *testing.T) {
	home := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv(RepoCacheRootEnv, "")

	got, err := RepoCacheRoot()
	if err != nil {
		t.Fatalf("RepoCacheRoot: %v", err)
	}
	want := filepath.Join(gcHome, "cache", "repos")
	if got != want {
		t.Fatalf("RepoCacheRoot = %q, want %q", got, want)
	}
}

func TestRepoCacheRootHonorsDedicatedOverride(t *testing.T) {
	home := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	override := filepath.Join(t.TempDir(), "isolated-cache")
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv(RepoCacheRootEnv, override)

	got, err := RepoCacheRoot()
	if err != nil {
		t.Fatalf("RepoCacheRoot: %v", err)
	}
	if got != override {
		t.Fatalf("RepoCacheRoot = %q, want override %q", got, override)
	}
}

func TestRepoCacheRootOverrideTakesPrecedenceOverGCHome(t *testing.T) {
	override := filepath.Join(t.TempDir(), "isolated-cache")
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "gc-home"))
	t.Setenv(RepoCacheRootEnv, override)

	got, err := RepoCacheRoot()
	if err != nil {
		t.Fatalf("RepoCacheRoot: %v", err)
	}
	if got != override {
		t.Fatalf("RepoCacheRoot = %q, want override %q", got, override)
	}
}

func TestRepoCacheRootValidAsLockRootCandidate(t *testing.T) {
	override := filepath.Join(t.TempDir(), "isolated-cache")
	t.Setenv(RepoCacheRootEnv, override)

	child := filepath.Join(override, "abc123", "pack.toml")
	root, ok := repoCacheRootForPath(child)
	if !ok {
		t.Fatalf("repoCacheRootForPath(%q) = not a known cache root, want %q", child, override)
	}
	abs, err := filepath.Abs(override)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	if root != abs {
		t.Fatalf("root = %q, want %q", root, abs)
	}
}
