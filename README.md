# Groundskeeper

Groundskeeper patches an orphaned script's own dependencies when a security
advisory is published against them, tests the patch in a sandbox, and merges
it directly when there's no one left to review it — then watches production
and rolls the patch back automatically if it turns out to be bad.

## The problem

An analyst on the marketing team wrote a small Python script eighteen months
ago: a cron job that reads a CSV export of campaign performance, computes a
summary, and posts it to Slack. It's `requests` and `pandas` and about 150
lines of straightforward pandas aggregation. It has run every night since,
unattended, quietly doing its job.

Then the analyst left the company.

Nobody remaining knows the script exists, let alone how to safely change it.
It has no CI, no code owner, and no Slack channel where someone would notice
if it broke. It works — until, six months later, a security advisory is
published against the exact version of `requests` it has pinned in
`requirements.txt`. Now the script is not just unmaintained, it's a
liability, and there is no human in the loop who can evaluate the fix, run
the tests, and merge the PR. The PR that a bot might open would simply sit
there forever, because the reviewer it's waiting for doesn't work here
anymore.

Groundskeeper exists to be that reviewer, within tightly scoped, sandboxed,
observable limits — for exactly this class of script: too small and too
orphaned to ever get proper ongoing maintenance, but still running, still
depended upon, and still exposed to the same dependency risk as anything
else in production.

## Disambiguation: this is not API-Patchbot

**Groundskeeper has nothing to do with detecting breaking changes in
external HTTP APIs that a script calls.** That is a different, unrelated
project idea — internally called **"API-Patchbot"** — and it is explicitly
out of scope here. If you're looking for a tool that watches a third-party
API for breaking changes and patches your client code accordingly, this
repository is the wrong one; that idea has not been built here at all.

Groundskeeper is about the opposite direction of the dependency graph: a
script's own **package dependencies** (the entries pinned in its
`requirements.txt`) going stale or gaining a published vulnerability, with
no human maintainer left to bump them safely. Everything in this repository
— the OSV/PyPI watcher, the sandboxed patch pipeline, the orphan detector,
the rollback state machine — is in service of that narrower, concrete
problem.

## Architecture overview

```
requirements.txt ──▶ Watcher (OSV + PyPI) ──▶ UpdateCandidate(s)
                                                     │
                                                     ▼
                                     Sandbox (ephemeral Docker container)
                                       - apply candidate to manifest
                                       - install deps
                                       - run repo's own test suite
                                                     │
                                        tests pass?  │  tests fail?
                                                     │        └──▶ stop, no patch lands
                                                     ▼
                                          Orphan check (internal/orphan)
                                        ┌──────────────┴──────────────┐
                                 orphaned?                      not orphaned?
                                        │                               │
                                        ▼                               ▼
                          Auto-merge directly (vcs)          Open PR, await human review
                                        │
                                        ▼
                       state.ApplyPatch (records last-known-good)
                                        │
                                        ▼
                      Monitor watches next N real runs (monitor)
                        ┌───────────────┴───────────────┐
                  all succeed?                    a run fails?
                        │                                │
                        ▼                                ▼
                 done, patch confirmed         state.Rollback + vcs revert PR
                                                       + notify fallback contact
```

See `docs/architecture.md` for the same flow with more detail per stage.

### Package layout

| Package                | Responsibility |
|-------------------------|----------------|
| `internal/watcher`      | Parses `requirements.txt`, queries OSV.dev and PyPI, produces prioritized `UpdateCandidate`s. |
| `internal/sandbox`      | Runs a candidate patch and the repo's own tests inside an ephemeral Docker container. |
| `internal/orphan`       | Decides whether a repo has had any human activity (commits, PR reviews) within a threshold window. |
| `internal/state`        | The centerpiece: persists `MaintenanceState` (last-known-good vs. active patch) to a JSON file, with `ApplyPatch` and `Rollback`. |
| `internal/monitor`      | Watches a target job's next several real runs after a patch lands; triggers `state.Rollback` on the first observed failure. |
| `internal/notify`       | Sends alerts to a fallback human contact (Slack incoming webhook implementation included). |
| `internal/vcs`          | Opens PRs, or merges directly when orphaned; opens/merges revert PRs on rollback. Built on `go-github`. |
| `cmd/groundskeeper`     | CLI: `watch`, `patch`, `status` subcommands wiring the above together. |
| `testdata/example-orphaned-script` | The demo fixture: a small, real, functional Python script mirroring the scenario above. |

**Scope note:** v1 targets Python repositories using `requirements.txt`
end-to-end. Other ecosystems (Node's `package.json`/`package-lock.json`, Go's
`go.mod`) would be implemented as additional adapters behind the same
`watcher`/`sandbox` interfaces, not built in this version. See
`docs/adr/0001-python-only-v1-scope.md` for the reasoning.

## Setup

This repository has been scaffolded but **never built, run, or tested**.
Nothing here has been verified to compile. To actually work with it:

```sh
# Resolve and lock real dependency versions (go.mod's require block was
# hand-authored with plausible-but-unverified versions and there is no
# go.sum yet).
go mod tidy

# Build all packages and the CLI binary.
go build ./...

# Run the unit tests (internal/orphan and internal/state have real,
# table-driven tests that are expected to pass once the module builds).
go test ./...
```

You will also need, for a real (non-scaffolded) deployment:

- A running Docker daemon reachable by the Docker Engine API, for
  `internal/sandbox`.
- A GitHub personal access token or App credentials with write access to the
  target repository, for `internal/vcs`.
- A Slack incoming webhook URL, for `internal/notify`, if you want alerts.

## Demo fixture walkthrough

`testdata/example-orphaned-script/` is a self-contained stand-in for the
analyst's cron job described above:

- `campaign_summary.py` reads a campaign-performance CSV, aggregates
  impressions/clicks/spend/conversions per campaign with `pandas`, and posts
  (or, with no webhook configured, prints) the summary — optionally via
  `requests` to a Slack webhook.
- `requirements.txt` pins `requests==2.28.1` and `pandas==1.5.3`, both
  deliberately a bit behind current releases.
- `tests/test_summary.py` is a real `pytest` suite exercising the CSV
  loading, aggregation, and message formatting logic against
  `fixtures/sample_campaign.csv`.

Walking through the end-to-end scenario this fixture supports:

1. **Advisory published.** A CVE lands against `requests==2.28.1`.
2. **Watcher detects it.** `groundskeeper watch --repo testdata/example-orphaned-script`
   parses `requirements.txt`, calls `POST https://api.osv.dev/v1/query` for
   each pin, finds the advisory against `requests`, checks PyPI for the
   latest release, and emits an `UpdateCandidate{Package: "requests",
   CurrentVersion: "2.28.1", TargetVersion: "<latest>", IsSecurityFix: true,
   AdvisoryID: "<advisory id>"}`.
3. **Sandbox patch pipeline runs.** `groundskeeper patch --repo
   testdata/example-orphaned-script --candidate requests@<latest>` spins up
   an ephemeral `python:3.11-slim` container, copies in the repo with
   `requirements.txt` rewritten to the new pin, runs `pip install -r
   requirements.txt` and then `pytest -q` against `tests/test_summary.py`.
4. **Orphan check.** `internal/orphan.IsOrphaned` looks at when a human last
   committed to or reviewed a PR on this repo. For the analyst's abandoned
   script, that's well past the 180-day threshold: orphaned.
5. **Auto-merge.** Because the repo is orphaned, `internal/vcs.SubmitPatch`
   opens the PR and merges it immediately instead of waiting for review.
   `internal/state.ApplyPatch` records the new commit as the active patch
   and the prior commit as last-known-good.
6. **Post-merge monitoring.** `internal/monitor.Monitor` watches the next
   few real nightly runs via `CronLogObserver`, looking for
   `GROUNDSKEEPER_RUN_STATUS=<exit code> at=<timestamp>` lines in the cron
   job's log output.
7. **A run fails.** Suppose the new `requests` release changed a default
   timeout behavior in a way the sandbox's `pytest` run didn't exercise, and
   the cron job now times out against Slack in production.
8. **Automatic rollback.** `Monitor` sees the failing run, calls
   `state.Rollback`, which reverts `MaintenanceState.ActivePatchCommit` back
   to `LastKnownGoodCommit`, and `internal/vcs.SubmitRevert` opens and merges
   a revert PR.
9. **Notification.** `internal/notify.SlackNotifier` posts a message to the
   configured fallback contact explaining exactly what happened and that the
   repo has been reverted to its last known good state.

No step above has actually been executed as part of building this
repository — see `docs/runbook.md` for the same walkthrough written as an
incident narrative, and the ADRs in `docs/adr/` for the reasoning behind the
two riskiest decisions (Python-only v1 scope, and auto-merging without human
review).
