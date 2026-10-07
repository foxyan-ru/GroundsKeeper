package state_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mstone/groundskeeper/internal/state"
	"github.com/mstone/groundskeeper/internal/watcher"
)

func statePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "groundskeeper-state.json")
}

// seed writes an initial MaintenanceState directly to path, simulating a
// repository Groundskeeper has already recorded a known-good baseline for
// (but has never patched).
func seed(t *testing.T, path string, s state.MaintenanceState) {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("seeding state file: marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("seeding state file: write: %v", err)
	}
}

func TestApplyPatchThenRollbackRoundTrips(t *testing.T) {
	path := statePath(t)
	seed(t, path, state.MaintenanceState{
		LastKnownGoodCommit: "commit-000-baseline",
		UpdatedAt:           time.Now().UTC(),
	})

	candidate := watcher.UpdateCandidate{
		Package:        "requests",
		CurrentVersion: "2.28.1",
		TargetVersion:  "2.31.0",
		IsSecurityFix:  true,
		AdvisoryID:     "GHSA-test-0001",
	}

	applied, err := state.ApplyPatch(path, "commit-101-patch", candidate)
	if err != nil {
		t.Fatalf("ApplyPatch returned unexpected error: %v", err)
	}
	if applied.LastKnownGoodCommit != "commit-000-baseline" {
		t.Errorf("after ApplyPatch, LastKnownGoodCommit = %q, want %q", applied.LastKnownGoodCommit, "commit-000-baseline")
	}
	if applied.ActivePatchCommit != "commit-101-patch" {
		t.Errorf("after ApplyPatch, ActivePatchCommit = %q, want %q", applied.ActivePatchCommit, "commit-101-patch")
	}
	if applied.ActivePatch == nil || applied.ActivePatch.Package != "requests" {
		t.Errorf("after ApplyPatch, ActivePatch = %+v, want a copy of the requests candidate", applied.ActivePatch)
	}

	// Confirm it was actually persisted, not just returned in memory.
	reloaded, err := state.Load(path)
	if err != nil {
		t.Fatalf("Load after ApplyPatch returned unexpected error: %v", err)
	}
	if reloaded.ActivePatchCommit != "commit-101-patch" {
		t.Errorf("reloaded ActivePatchCommit = %q, want %q", reloaded.ActivePatchCommit, "commit-101-patch")
	}

	rolledBack, err := state.Rollback(path)
	if err != nil {
		t.Fatalf("Rollback returned unexpected error: %v", err)
	}
	if rolledBack.ActivePatchCommit != "" {
		t.Errorf("after Rollback, ActivePatchCommit = %q, want empty", rolledBack.ActivePatchCommit)
	}
	if rolledBack.ActivePatch != nil {
		t.Errorf("after Rollback, ActivePatch = %+v, want nil", rolledBack.ActivePatch)
	}
	if rolledBack.LastKnownGoodCommit != "commit-000-baseline" {
		t.Errorf("after Rollback, LastKnownGoodCommit = %q, want %q", rolledBack.LastKnownGoodCommit, "commit-000-baseline")
	}

	// And again, confirm the rollback itself was persisted.
	reloadedAfterRollback, err := state.Load(path)
	if err != nil {
		t.Fatalf("Load after Rollback returned unexpected error: %v", err)
	}
	if reloadedAfterRollback.ActivePatchCommit != "" || reloadedAfterRollback.ActivePatch != nil {
		t.Errorf("reloaded state after rollback still shows an active patch: %+v", reloadedAfterRollback)
	}
}

func TestApplyPatchTwiceAdvancesLastKnownGood(t *testing.T) {
	path := statePath(t)
	candidate1 := watcher.UpdateCandidate{Package: "pandas", CurrentVersion: "1.5.3", TargetVersion: "1.5.4"}
	candidate2 := watcher.UpdateCandidate{Package: "requests", CurrentVersion: "2.28.1", TargetVersion: "2.31.0", IsSecurityFix: true}

	if _, err := state.ApplyPatch(path, "commit-A", candidate1); err != nil {
		t.Fatalf("first ApplyPatch returned unexpected error: %v", err)
	}

	second, err := state.ApplyPatch(path, "commit-B", candidate2)
	if err != nil {
		t.Fatalf("second ApplyPatch returned unexpected error: %v", err)
	}
	if second.LastKnownGoodCommit != "commit-A" {
		t.Errorf("after second ApplyPatch, LastKnownGoodCommit = %q, want %q (the first patch's commit)", second.LastKnownGoodCommit, "commit-A")
	}
	if second.ActivePatchCommit != "commit-B" {
		t.Errorf("after second ApplyPatch, ActivePatchCommit = %q, want %q", second.ActivePatchCommit, "commit-B")
	}
}

func TestRollbackWithNoActivePatchReturnsClearError(t *testing.T) {
	path := statePath(t)

	// No state file exists at all yet.
	if _, err := state.Rollback(path); !errors.Is(err, state.ErrNothingToRollBack) {
		t.Errorf("Rollback on nonexistent state = %v, want ErrNothingToRollBack", err)
	}

	// A state file exists but has never had a patch applied.
	seed(t, path, state.MaintenanceState{
		LastKnownGoodCommit: "commit-000-baseline",
		UpdatedAt:           time.Now().UTC(),
	})
	if _, err := state.Rollback(path); !errors.Is(err, state.ErrNothingToRollBack) {
		t.Errorf("Rollback with no active patch = %v, want ErrNothingToRollBack", err)
	}
}

func TestRollingBackTwiceInARowFailsTheSecondTime(t *testing.T) {
	path := statePath(t)
	candidate := watcher.UpdateCandidate{Package: "pandas", CurrentVersion: "1.5.3", TargetVersion: "1.5.4"}

	if _, err := state.ApplyPatch(path, "commit-A", candidate); err != nil {
		t.Fatalf("ApplyPatch returned unexpected error: %v", err)
	}
	if _, err := state.Rollback(path); err != nil {
		t.Fatalf("first Rollback returned unexpected error: %v", err)
	}
	if _, err := state.Rollback(path); !errors.Is(err, state.ErrNothingToRollBack) {
		t.Errorf("second consecutive Rollback = %v, want ErrNothingToRollBack", err)
	}
}
