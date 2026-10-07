# Runbook: a security advisory against an orphaned script's dependency

This runbook narrates the scenario Groundskeeper is built for as a step-by-
step incident, using the demo fixture at
`testdata/example-orphaned-script/` as the concrete example. It exists both
as documentation of intended behavior and as the reference walkthrough for
anyone driving a live demo of this project.

## Background

`campaign_summary.py` is a nightly cron job. It reads a CSV export of
campaign performance, computes per-campaign metrics, and posts a summary to
Slack. The analyst who wrote it left the company five months ago. It has
run every night since without incident, and without anyone looking at it.

Its pinned dependencies (`testdata/example-orphaned-script/requirements.txt`):

```
requests==2.28.1
pandas==1.5.3
```

## Timeline

### T+0: Advisory published

A CVE is published against `requests==2.28.1`. Nobody at the company reads
security advisories for a script they don't know exists.

### T+~1h: Watcher detects it

Groundskeeper's scheduled watch run executes:

```
groundskeeper watch --repo /repos/campaign-summary-cron
```

Internally, `internal/watcher.Watcher.Check` parses
`requirements.txt` into `PinnedDependency{Package: "requests", Version:
"2.28.1"}` (among others), then for each pin:

1. `POST https://api.osv.dev/v1/query` with
   `{"version": "2.28.1", "package": {"name": "requests", "ecosystem": "PyPI"}}`.
   The response includes a vulnerability entry with `ID: "GHSA-xxxx-xxxx-xxxx"`.
2. `GET https://pypi.org/pypi/requests/json` to find the latest available
   release, say `2.31.0`.

The watcher emits:

```go
UpdateCandidate{
    Package:        "requests",
    CurrentVersion: "2.28.1",
    TargetVersion:  "2.31.0",
    IsSecurityFix:  true,
    AdvisoryID:     "GHSA-xxxx-xxxx-xxxx",
}
```

sorted ahead of any non-security update candidates found for `pandas`.

### T+~1h5m: Sandbox patch pipeline runs

Groundskeeper (or an operator, via `groundskeeper patch --repo
/repos/campaign-summary-cron --candidate requests@2.31.0`) hands the
candidate to `internal/sandbox.Sandbox.Run`:

1. An ephemeral `python:3.11-slim` container is created and started.
2. A fresh checkout of the repo is copied in via `CopyToContainer`, with
   `requirements.txt` rewritten in-flight (via
   `ApplyCandidateToRequirements`) so the line reads `requests==2.31.0`
   instead of `requests==2.28.1` — every other line, including the
   `pandas==1.5.3` pin and any comments, is left untouched.
3. `pip install -r requirements.txt` runs inside the container.
4. `pytest -q` runs `tests/test_summary.py` against `fixtures/
   sample_campaign.csv`.
5. All tests pass. `PatchAttemptResult{Passed: true, ...}` is returned.
   The ephemeral container is torn down immediately afterward regardless of
   outcome.

### T+~1h6m: Orphan check

`internal/orphan.IsOrphaned` is evaluated against this repository's
`ActivitySignal`:

- `LastHumanCommitAt`: 5 months ago (the analyst's last commit before
  leaving).
- `LastPRReviewAt`: never recorded.

Against the configured 180-day threshold, this repo qualifies as orphaned
(more precisely: whichever signal is *most recent* — here, the commit — is
still well past 5 months; if it were past the 180-day line the repo is
orphaned, and here it will be shortly, or already is, depending on exactly
when the analyst's last commit landed).

### T+~1h7m: Auto-merge

Because the repo is orphaned and the sandbox run passed,
`internal/vcs.GitHubVCS.SubmitPatch` is called with `orphaned = true`. It:

1. Opens a PR titled `groundskeeper: bump requests 2.28.1 -> 2.31.0
   (security fix) [GHSA-xxxx-xxxx-xxxx]`, with a body describing the
   advisory and linking the sandbox test logs.
2. Immediately merges that PR via squash merge, with a merge message
   explaining that this repo had no human activity within the orphan
   threshold and so was merged directly rather than left for a review that
   would never come.

`internal/state.ApplyPatch` records the result:

```json
{
  "last_known_good_commit": "a1b2c3d (pre-patch HEAD)",
  "active_patch_commit": "e4f5g6h (merge commit)",
  "active_patch": {
    "package": "requests",
    "current_version": "2.28.1",
    "target_version": "2.31.0",
    "is_security_fix": true,
    "advisory_id": "GHSA-xxxx-xxxx-xxxx"
  },
  "updated_at": "2026-09-28T01:15:00Z"
}
```

### T+~1 day: Next run fails

That night, cron runs `campaign_summary.py` as usual. Suppose the new
`requests` release changed a default timeout behavior in a way that the
sandbox's `pytest` run — which never actually calls the network — had no way
to exercise. The script's `post_to_slack` call to the real Slack webhook now
times out, and the wrapper around the script logs:

```
GROUNDSKEEPER_RUN_STATUS=1 at=2026-09-29T03:00:07Z
```

`internal/monitor.CronLogObserver.ObserveNextRun`, tailing that log, parses
this line and returns `RunResult{Succeeded: false, ExitCode: 1, ObservedAt:
2026-09-29T03:00:07Z}`.

### T+~1 day, immediately after: Automatic rollback

`internal/monitor.Monitor.WatchAfterPatch`, which had been waiting on
exactly this observation, reacts to the first failure without waiting for
any further runs:

1. Calls `internal/state.Rollback`, which clears `ActivePatchCommit` and
   `ActivePatch`, restoring the repo's recorded state to
   `LastKnownGoodCommit: "a1b2c3d"`.
2. Calls `internal/vcs.GitHubVCS.SubmitRevert`, which opens and immediately
   merges a PR titled `Revert: groundskeeper: bump requests 2.28.1 ->
   2.31.0 (security fix) [GHSA-xxxx-xxxx-xxxx]`, explaining the observed
   failure.

### T+~1 day, seconds later: Notification

`internal/monitor.Monitor` calls `internal/notify.Notifier.Notify` (backed
by `internal/notify.SlackNotifier` posting to the incoming webhook
configured for this repo's fallback contact):

> Groundskeeper: run 1/3 after patch requests 2.28.1 -> 2.31.0
> (GHSA-xxxx-xxxx-xxxx) failed (exit code 1, observed at
> 2026-09-29T03:00:07Z). Automatically rolled back to last-known-good
> commit a1b2c3d.

A human — the fallback contact configured when this repo was onboarded to
Groundskeeper, not the departed analyst — now knows exactly what happened,
why, and that the script is already back to a known-working state. The
vulnerability from `GHSA-xxxx-xxxx-xxxx` is once again unpatched, but the
script is running, and a human has been handed a concrete, actionable
starting point instead of silence.

## What "done" looks like for this incident

- The advisory was detected without a human watching for it.
- A fix was attempted, tested in isolation, and landed without waiting on a
  reviewer who doesn't exist.
- When the fix turned out to be incomplete, production was protected within
  one failed run, automatically, with a full audit trail (PR, revert PR,
  state file, notification) — not by a human noticing something was broken
  days later.
- A human was told, with enough context to actually act, instead of being
  expected to have been watching a script they didn't know existed.
