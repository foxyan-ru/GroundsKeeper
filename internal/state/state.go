// Package state persists Groundskeeper's maintenance state for a target
// repository to a JSON file on disk, in the same spirit as a cutover/
// rollback state file used to manage a blue-green deployment: it always
// knows what the last-known-good commit was, and can revert to it
// deterministically if a newly-applied patch turns out to be bad.
//
// This is the centerpiece of Groundskeeper's safety story. Every other
// package (sandbox, monitor, vcs) exists to either produce inputs to
// ApplyPatch or to react to the state it records.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mstone/groundskeeper/internal/watcher"
)

// ErrNothingToRollBack is returned by Rollback when the state file has no
// active patch recorded (either because no patch has ever been applied, or
// because a previous rollback already cleared it). Rolling back twice in a
// row, or rolling back a repo Groundskeeper has never touched, is a bug in
// the caller, not a silent no-op.
var ErrNothingToRollBack = errors.New("state: no active patch to roll back")

// MaintenanceState is Groundskeeper's full view of a target repository's
// patch lifecycle at a point in time. It is serialized as JSON and persisted
// to a single state file per target repository.
type MaintenanceState struct {
	// LastKnownGoodCommit is the commit SHA the target repo was at
	// immediately before the currently-active patch (or, if no patch is
	// active, simply the last commit Groundskeeper verified was healthy).
	LastKnownGoodCommit string `json:"last_known_good_commit"`

	// ActivePatchCommit is the commit SHA introduced by the currently
	// active patch, or empty if no patch is currently active.
	ActivePatchCommit string `json:"active_patch_commit,omitempty"`

	// ActivePatch describes the dependency bump the active commit applied,
	// or nil if no patch is currently active.
	ActivePatch *watcher.UpdateCandidate `json:"active_patch,omitempty"`

	// UpdatedAt records when this state was last written.
	UpdatedAt time.Time `json:"updated_at"`
}

// Load reads and parses the MaintenanceState stored at path. A missing file
// is not an error: it returns a zero-value MaintenanceState, representing a
// repository Groundskeeper has not yet recorded any state for.
func Load(path string) (*MaintenanceState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &MaintenanceState{}, nil
		}
		return nil, fmt.Errorf("state: reading state file %s: %w", path, err)
	}
	var s MaintenanceState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("state: parsing state file %s: %w", path, err)
	}
	return &s, nil
}

// save writes s to path as pretty-printed JSON, creating parent directories
// as needed, and writing atomically via a temp-file-then-rename so a crash
// mid-write can never leave a half-written, corrupt state file behind.
func save(path string, s *MaintenanceState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("state: creating state directory: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshaling state: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".groundskeeper-state-*.tmp")
	if err != nil {
		return fmt.Errorf("state: creating temp state file: %w", err)
	}
	tmpPath := tmp.Name()
	// Ensure the temp file is cleaned up if anything below fails before the
	// rename takes ownership of it.
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("state: writing temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: closing temp state file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("state: renaming temp state file into place: %w", err)
	}
	return nil
}

// ApplyPatch records that newCommit (which applies candidate on top of the
// repo's current state) is now the active patch. Whatever commit was
// recorded as active immediately beforehand is remembered as the new
// last-known-good commit to roll back to if newCommit turns out to be bad;
// if no patch was previously active, the existing LastKnownGoodCommit (the
// repo's baseline before Groundskeeper ever touched it) is carried forward
// unchanged.
//
// ApplyPatch reads whatever state currently exists at path (via Load) so
// that the "prior commit" it remembers is always whatever Groundskeeper
// itself last recorded as active — callers never need to track that
// separately.
func ApplyPatch(path, newCommit string, candidate watcher.UpdateCandidate) (*MaintenanceState, error) {
	if newCommit == "" {
		return nil, errors.New("state: ApplyPatch requires a non-empty newCommit")
	}

	current, err := Load(path)
	if err != nil {
		return nil, err
	}

	lastKnownGood := current.LastKnownGoodCommit
	if current.ActivePatchCommit != "" {
		// A patch was already active; it's being superseded by newCommit,
		// so it becomes the new rollback target.
		lastKnownGood = current.ActivePatchCommit
	}

	candidateCopy := candidate
	newState := &MaintenanceState{
		LastKnownGoodCommit: lastKnownGood,
		ActivePatchCommit:   newCommit,
		ActivePatch:         &candidateCopy,
		UpdatedAt:           time.Now().UTC(),
	}

	if err := save(path, newState); err != nil {
		return nil, err
	}
	return newState, nil
}

// Rollback reverts the state recorded at path so that ActivePatchCommit is
// cleared and the repository is considered to be back at
// LastKnownGoodCommit. It returns ErrNothingToRollBack if there is no
// active patch recorded to roll back from.
//
// Rollback does not itself touch the target repository's git history or
// working tree — that is the responsibility of internal/vcs, which is
// expected to check out/revert to the returned LastKnownGoodCommit. This
// function only updates Groundskeeper's bookkeeping of what state it
// believes the repo should be in.
func Rollback(path string) (*MaintenanceState, error) {
	current, err := Load(path)
	if err != nil {
		return nil, err
	}
	if current.ActivePatchCommit == "" && current.ActivePatch == nil {
		return nil, ErrNothingToRollBack
	}

	newState := &MaintenanceState{
		LastKnownGoodCommit: current.LastKnownGoodCommit,
		ActivePatchCommit:   "",
		ActivePatch:         nil,
		UpdatedAt:           time.Now().UTC(),
	}

	if err := save(path, newState); err != nil {
		return nil, err
	}
	return newState, nil
}
