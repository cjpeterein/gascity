package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

// TestSupervisorCacheReadClient covers the cache-read routing used by hot
// read-only paths (gc hook, gc mail check): apiClient when it resolves, the
// supervisor-managed client when the controller socket is alive without a
// standalone [api] port, and nil under GC_NO_API or with no controller at
// all. Mirrors the maintenance fall-through (ga-tp7) for consumers that have
// a local fallback but prefer supervisor-cache reads.
func TestSupervisorCacheReadClient(t *testing.T) {
	sentinel := api.NewClient("http://supervisor.sentinel:1")
	origAlive, origSup := apiRouteControllerAliveHook, apiRouteSupervisorClientHook
	t.Cleanup(func() {
		apiRouteControllerAliveHook = origAlive
		apiRouteSupervisorClientHook = origSup
	})

	t.Run("alive-no-api-port-routes-to-supervisor", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		apiRouteControllerAliveHook = func(string) int { return 4242 }
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if got := supervisorCacheReadClient(dir); got != sentinel {
			t.Fatalf("supervisorCacheReadClient = %p, want supervisor sentinel %p", got, sentinel)
		}
	})

	t.Run("alive-with-api-port-uses-standalone", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		apiRouteControllerAliveHook = func(string) int { return 4242 }
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n[api]\nport = 8080\n")
		got := supervisorCacheReadClient(dir)
		if got == nil {
			t.Fatalf("supervisorCacheReadClient = nil, want standalone client")
		}
		if got == sentinel {
			t.Fatalf("supervisorCacheReadClient returned supervisor sentinel, want standalone client")
		}
	})

	t.Run("no-controller-returns-supervisor-or-nil", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		apiRouteControllerAliveHook = func(string) int { return 0 }
		apiRouteSupervisorClientHook = func(string) *api.Client { return nil }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if got := supervisorCacheReadClient(dir); got != nil {
			t.Fatalf("supervisorCacheReadClient = %p, want nil with no controller and no supervisor", got)
		}
	})

	t.Run("escape-hatch-returns-nil", func(t *testing.T) {
		t.Setenv("GC_NO_API", "1")
		apiRouteControllerAliveHook = func(string) int { return 4242 }
		apiRouteSupervisorClientHook = func(string) *api.Client { return sentinel }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		if got := supervisorCacheReadClient(dir); got != nil {
			t.Fatalf("supervisorCacheReadClient = %p, want nil under GC_NO_API", got)
		}
	})
}
