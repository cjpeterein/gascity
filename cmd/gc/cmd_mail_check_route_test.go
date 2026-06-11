package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

// TestMailCheckAPIClientRoutesToSupervisor proves the status-line mail-check
// path reaches the supervisor API on a supervisor-managed city (controller
// socket alive, no standalone [api] port). Before gc-lqt this returned
// (nil, controller-down) and every status-line render fell back to direct
// mail-provider reads against the data plane.
func TestMailCheckAPIClientRoutesToSupervisor(t *testing.T) {
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
		c, reason := mailCheckAPIClient(dir)
		if c != sentinel {
			t.Fatalf("mailCheckAPIClient client = %p, want supervisor sentinel %p", c, sentinel)
		}
		if reason != "" {
			t.Fatalf("mailCheckAPIClient reason = %q, want empty", reason)
		}
	})

	t.Run("no-endpoint-reports-fallback-reason", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		apiRouteControllerAliveHook = func(string) int { return 0 }
		apiRouteSupervisorClientHook = func(string) *api.Client { return nil }
		dir := writeCityTOMLForRoute(t, t.TempDir(), "name = \"t\"\n")
		c, reason := mailCheckAPIClient(dir)
		if c != nil {
			t.Fatalf("mailCheckAPIClient client = %p, want nil", c)
		}
		if reason == "" {
			t.Fatalf("mailCheckAPIClient reason = empty, want a fallback reason code")
		}
	})
}
