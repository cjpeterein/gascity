package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func TestBuildDoctorChecks_NameSetUnchanged(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	checks := buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    false,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	})
	names := doctorCheckNames(checks)

	data, err := os.ReadFile(filepath.Join("testdata", "doctor_check_names.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	want := strings.TrimSpace(string(data))
	got := strings.Join(names, "\n")
	if got != want {
		t.Fatalf("doctor check names changed\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestBuildDoctorChecksRegistersNamedAlwaysMinConflictCheck(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	names := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    false,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	}))

	formulaRequirements := doctorCheckIndex(names, "formula-requirements")
	if formulaRequirements < 0 {
		t.Fatalf("formula-requirements check missing: %v", names)
	}
	got := doctorCheckIndex(names, "named-always-min-conflict")
	if got != formulaRequirements+1 {
		t.Fatalf("named-always-min-conflict index = %d, want immediately after formula-requirements at %d; names=%v", got, formulaRequirements, names)
	}
}

func TestBuildDoctorChecksSkipsNamedAlwaysMinConflictCheckWithoutConfig(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")

	tests := []struct {
		name   string
		cfg    *config.City
		cfgErr error
	}{
		{name: "nil config", cfg: nil, cfgErr: nil},
		{name: "config load error", cfg: &config.City{Workspace: config.Workspace{Name: "demo"}}, cfgErr: os.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			names := doctorCheckNames(buildDoctorChecks(cityDir, tt.cfg, tt.cfgErr, buildDoctorChecksOpts{
				ControllerRunning:    false,
				SkipCityDoltCheck:    true,
				SkipManagedDoltCheck: true,
			}))
			if got := doctorCheckIndex(names, "named-always-min-conflict"); got >= 0 {
				t.Fatalf("named-always-min-conflict registered at %d, want absent; names=%v", got, names)
			}
		})
	}
}

// TestBuildDoctorChecksRegistersCityDoltLocalOnlyForManagedCity verifies the
// city-store variant of the dolt local-only remote check registers when the
// city scope uses the managed bd store contract. The city's own database is
// not in cfg.Rigs, so the per-rig loop cannot provide this coverage
// (gc-gic4m: stray off-box remote on the hq city store went undetected).
func TestBuildDoctorChecksRegistersCityDoltLocalOnlyForManagedCity(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n\n[beads]\nprovider = \"bd\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	names := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    false,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	}))

	if doctorCheckIndex(names, "city:dolt-local-only-remote") < 0 {
		t.Fatalf("city:dolt-local-only-remote missing for managed-bd city; names=%v", names)
	}
}

// TestBuildDoctorChecksSkipsCityDoltLocalOnly verifies the city-store
// variant stays unregistered for non-managed cities and GC_DOLT=skip
// environments, matching the per-rig gating.
func TestBuildDoctorChecksSkipsCityDoltLocalOnly(t *testing.T) {
	tests := []struct {
		name     string
		cityToml string
		gcDolt   string
	}{
		{
			name:     "file-backed city",
			cityToml: "[workspace]\nname = \"demo\"\n\n[beads]\nprovider = \"file\"\n",
			gcDolt:   "",
		},
		{
			name:     "GC_DOLT skip",
			cityToml: "[workspace]\nname = \"demo\"\n\n[beads]\nprovider = \"bd\"\n",
			gcDolt:   "skip",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(tt.cityToml), 0o644); err != nil {
				t.Fatalf("write city.toml: %v", err)
			}
			t.Setenv("GC_DOLT", tt.gcDolt)
			cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

			names := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
				ControllerRunning:    false,
				SkipCityDoltCheck:    true,
				SkipManagedDoltCheck: true,
			}))

			if got := doctorCheckIndex(names, "city:dolt-local-only-remote"); got >= 0 {
				t.Fatalf("city:dolt-local-only-remote registered at %d, want absent; names=%v", got, names)
			}
		})
	}
}

func doctorCheckNames(checks []doctor.Check) []string {
	names := make([]string, 0, len(checks))
	for _, check := range checks {
		names = append(names, check.Name())
	}
	return names
}

func doctorCheckIndex(names []string, want string) int {
	for i, name := range names {
		if name == want {
			return i
		}
	}
	return -1
}
