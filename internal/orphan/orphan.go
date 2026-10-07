// Package orphan decides whether a target repository has been abandoned by
// its human maintainers, based on how long it has been since any human
// activity (a commit or a PR review) was observed on it.
//
// This determination is what allows Groundskeeper to justify auto-merging a
// sandboxed, test-passing security patch instead of opening a pull request
// and waiting for a review that, on an orphaned repo, will never come. See
// docs/adr/0002-auto-merge-for-orphaned-repos.md for the full reasoning.
package orphan

import "time"

// ActivitySignal captures the most recent human activity Groundskeeper was
// able to observe on a target repository. Both fields are the zero
// time.Time when no such activity has ever been observed (e.g. a repo with
// no PRs at all), which IsOrphaned treats as "infinitely long ago".
type ActivitySignal struct {
	// LastHumanCommitAt is the timestamp of the most recent commit
	// authored by a human (as opposed to a bot, including Groundskeeper
	// itself) on the repository's default branch.
	LastHumanCommitAt time.Time

	// LastPRReviewAt is the timestamp of the most recent human-submitted
	// pull request review (approval, comment, or change request) on the
	// repository.
	LastPRReviewAt time.Time
}

// mostRecent returns the later of the two timestamps in signal, treating a
// zero time.Time as "never happened" rather than as a valid, very-old
// timestamp that could accidentally win a max() comparison in the caller's
// favor.
func (s ActivitySignal) mostRecent() (time.Time, bool) {
	switch {
	case s.LastHumanCommitAt.IsZero() && s.LastPRReviewAt.IsZero():
		return time.Time{}, false
	case s.LastHumanCommitAt.IsZero():
		return s.LastPRReviewAt, true
	case s.LastPRReviewAt.IsZero():
		return s.LastHumanCommitAt, true
	case s.LastHumanCommitAt.After(s.LastPRReviewAt):
		return s.LastHumanCommitAt, true
	default:
		return s.LastPRReviewAt, true
	}
}

// IsOrphaned reports whether a repository should be considered orphaned as
// of now, given the most recent human activity observed in signal and a
// staleness threshold (e.g. 180 days).
//
// A repository is orphaned when neither a human commit nor a human PR
// review has been observed within the threshold window. A repo with no
// recorded activity at all (both fields zero) is always considered
// orphaned, since Groundskeeper has no evidence anyone is watching it.
//
// Activity exactly `threshold` old is considered NOT orphaned yet (the
// boundary is inclusive of "still active") — see the table-driven test for
// the exact edge-case behavior.
func IsOrphaned(signal ActivitySignal, threshold time.Duration) bool {
	return IsOrphanedAt(signal, threshold, time.Now())
}

// IsOrphanedAt is the same check as IsOrphaned but with the "current time"
// injected explicitly, so callers (and tests) don't depend on wall-clock
// time.
func IsOrphanedAt(signal ActivitySignal, threshold time.Duration, now time.Time) bool {
	last, ok := signal.mostRecent()
	if !ok {
		// No activity has ever been observed; treat as orphaned.
		return true
	}
	age := now.Sub(last)
	return age > threshold
}
