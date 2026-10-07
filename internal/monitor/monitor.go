// Package monitor watches a target repository's next several real runs
// after Groundskeeper has auto-merged a patch, so that a regression the
// sandboxed test suite didn't catch can still be caught and reverted before
// it does real damage.
//
// This is what makes auto-merging onto an orphaned repo (see
// docs/adr/0002-auto-merge-for-orphaned-repos.md) an acceptable risk instead
// of a reckless one: the safety net doesn't end at "tests passed in the
// sandbox," it continues into production.
package monitor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/mstone/groundskeeper/internal/state"
)

// RunResult describes the outcome of a single observed real run of the
// target script/job.
type RunResult struct {
	Succeeded  bool
	ExitCode   int
	ObservedAt time.Time
}

// RunObserver knows how to wait for and report on the next real run of the
// target job. Implementations are expected to block (respecting ctx) until
// either a new run's outcome is available or ctx is done.
type RunObserver interface {
	ObserveNextRun(ctx context.Context) (RunResult, error)
}

// --- CronLogObserver --------------------------------------------------
//
// CronLogObserver watches a cron job's log output for a status line in the
// format:
//
//	GROUNDSKEEPER_RUN_STATUS=<exit_code> at=<RFC3339 timestamp>
//
// e.g.:
//
//	GROUNDSKEEPER_RUN_STATUS=0 at=2026-09-28T03:00:12Z
//	GROUNDSKEEPER_RUN_STATUS=1 at=2026-09-29T03:00:07Z
//
// A real deployment would have the target script (or a thin wrapper around
// it) emit this line itself on every run, giving Groundskeeper a
// zero-ambiguity signal instead of trying to infer success/failure from
// arbitrary log content.

const runStatusPrefix = "GROUNDSKEEPER_RUN_STATUS="

// ErrNoRunStatusFound is returned when a log stream is exhausted without
// ever producing a recognizable run-status line.
var ErrNoRunStatusFound = fmt.Errorf("monitor: no %s line found in log stream", strings.TrimSuffix(runStatusPrefix, "="))

// LogTailer supplies the next chunk of a cron job's log output.
// A real implementation might tail a file, poll a log-aggregation API, or
// subscribe to a syslog stream; it is injected so CronLogObserver's parsing
// logic can be tested without any of that infrastructure.
type LogTailer interface {
	// NextLines blocks (respecting ctx) until at least one new log line is
	// available since the last call, then returns it/them. Returns io.EOF
	// once the log source is known to have no more output.
	NextLines(ctx context.Context) ([]string, error)
}

// CronLogObserver implements RunObserver by tailing a cron job's log output
// via a LogTailer and parsing out GROUNDSKEEPER_RUN_STATUS lines.
type CronLogObserver struct {
	Tailer LogTailer
}

// ObserveNextRun reads log lines from o.Tailer until it finds a
// GROUNDSKEEPER_RUN_STATUS line, parses it, and returns the corresponding
// RunResult.
func (o *CronLogObserver) ObserveNextRun(ctx context.Context) (RunResult, error) {
	for {
		select {
		case <-ctx.Done():
			return RunResult{}, ctx.Err()
		default:
		}

		lines, err := o.Tailer.NextLines(ctx)
		if err != nil {
			if err == io.EOF {
				return RunResult{}, ErrNoRunStatusFound
			}
			return RunResult{}, fmt.Errorf("monitor: reading log lines: %w", err)
		}

		for _, line := range lines {
			result, ok, parseErr := parseRunStatusLine(line)
			if parseErr != nil {
				return RunResult{}, parseErr
			}
			if ok {
				return result, nil
			}
		}
	}
}

// parseRunStatusLine attempts to parse a single log line as a
// GROUNDSKEEPER_RUN_STATUS marker. ok is false (with a nil error) when the
// line simply isn't a status line, so callers can keep scanning.
func parseRunStatusLine(line string) (result RunResult, ok bool, err error) {
	idx := strings.Index(line, runStatusPrefix)
	if idx < 0 {
		return RunResult{}, false, nil
	}
	rest := line[idx+len(runStatusPrefix):]

	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return RunResult{}, false, fmt.Errorf("monitor: malformed run-status line (no fields): %q", line)
	}

	exitCode, err := strconv.Atoi(fields[0])
	if err != nil {
		return RunResult{}, false, fmt.Errorf("monitor: malformed run-status line (bad exit code %q): %q", fields[0], line)
	}

	var observedAt time.Time
	for _, field := range fields[1:] {
		const tsPrefix = "at="
		if strings.HasPrefix(field, tsPrefix) {
			ts, parseErr := time.Parse(time.RFC3339, strings.TrimPrefix(field, tsPrefix))
			if parseErr != nil {
				return RunResult{}, false, fmt.Errorf("monitor: malformed run-status timestamp %q: %q", field, line)
			}
			observedAt = ts
		}
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}

	return RunResult{
		Succeeded:  exitCode == 0,
		ExitCode:   exitCode,
		ObservedAt: observedAt,
	}, true, nil
}

// LineReaderTailer is a simple LogTailer over any io.Reader (e.g. an
// os.File being tailed, or a bytes.Reader in tests), returning one line per
// NextLines call.
type LineReaderTailer struct {
	scanner *bufio.Scanner
}

// NewLineReaderTailer wraps r as a LogTailer.
func NewLineReaderTailer(r io.Reader) *LineReaderTailer {
	return &LineReaderTailer{scanner: bufio.NewScanner(r)}
}

// NextLines returns the next single line from the wrapped reader.
func (t *LineReaderTailer) NextLines(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !t.scanner.Scan() {
		if err := t.scanner.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	return []string{t.scanner.Text()}, nil
}

// --- Monitor --------------------------------------------------------------

// Notifier is the minimal surface Monitor needs from internal/notify,
// declared locally to avoid an import cycle and to keep Monitor trivially
// testable with a fake.
type Notifier interface {
	Notify(ctx context.Context, message string) error
}

// Monitor watches up to MaxObservedRuns real runs of the target job after a
// patch has been applied. If any observed run fails, it automatically rolls
// back the recorded MaintenanceState and notifies the configured fallback
// contact. If every observed run succeeds, the patch is considered
// confirmed healthy and no action is taken.
type Monitor struct {
	Observer RunObserver
	Notifier Notifier

	// StatePath is the Groundskeeper state file for the target repo, as
	// used by internal/state.
	StatePath string

	// MaxObservedRuns bounds how many successful runs Monitor waits for
	// before considering a patch confirmed healthy. Defaults to 3 if zero.
	MaxObservedRuns int
}

// WatchAfterPatch observes up to m.MaxObservedRuns real runs. On the first
// failing run it calls state.Rollback(m.StatePath) and sends a notification
// describing what happened, then returns immediately without waiting for
// further runs. If every observed run succeeds, it returns nil without
// touching state.
//
// patchDescription is a short human-readable description of the applied
// patch (e.g. "requests 2.28.1 -> 2.31.0 (GHSA-test-0001)"), used only to
// make the notification message legible; callers typically build it from a
// watcher.UpdateCandidate.
func (m *Monitor) WatchAfterPatch(ctx context.Context, patchDescription string) error {
	maxRuns := m.MaxObservedRuns
	if maxRuns <= 0 {
		maxRuns = 3
	}

	for i := 1; i <= maxRuns; i++ {
		result, err := m.Observer.ObserveNextRun(ctx)
		if err != nil {
			return fmt.Errorf("monitor: observing run %d/%d: %w", i, maxRuns, err)
		}

		if result.Succeeded {
			continue
		}

		// The patch broke something the sandboxed test suite didn't catch.
		// Roll back immediately rather than waiting for the remaining runs.
		rolledBack, rollbackErr := state.Rollback(m.StatePath)
		notifyMsg := fmt.Sprintf(
			"Groundskeeper: run %d/%d after patch %s failed (exit code %d, observed at %s). ",
			i, maxRuns, patchDescription, result.ExitCode, result.ObservedAt.Format(time.RFC3339),
		)
		if rollbackErr != nil {
			notifyMsg += fmt.Sprintf("Automatic rollback FAILED: %v. Manual intervention required immediately.", rollbackErr)
			if notifyErr := m.Notifier.Notify(ctx, notifyMsg); notifyErr != nil {
				return fmt.Errorf("monitor: rollback failed (%v) and notification also failed: %w", rollbackErr, notifyErr)
			}
			return fmt.Errorf("monitor: rollback failed after observing broken run: %w", rollbackErr)
		}

		notifyMsg += fmt.Sprintf(
			"Automatically rolled back to last-known-good commit %s.",
			rolledBack.LastKnownGoodCommit,
		)
		if notifyErr := m.Notifier.Notify(ctx, notifyMsg); notifyErr != nil {
			return fmt.Errorf("monitor: rolled back successfully but failed to notify: %w", notifyErr)
		}
		return nil
	}

	return nil
}
