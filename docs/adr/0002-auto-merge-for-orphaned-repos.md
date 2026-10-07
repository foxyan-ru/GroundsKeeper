# ADR 0002: Auto-merge patches directly on repos flagged as orphaned

## Status

Accepted.

## Context

Groundskeeper's normal, conservative behavior for a security patch is to
open a pull request and let a human review and merge it. That's the right
default. It is also useless for the exact repositories Groundskeeper exists
to help: a script whose author left the company, with no other engineer
assigned to it, no on-call rotation watching it, and no Slack channel where
a PR notification would land in front of anyone who'd act on it.

For an orphaned repo, "open a PR and wait for review" is not actually a
more cautious behavior than auto-merging — it is a slower way of doing
nothing. The PR sits open indefinitely. The vulnerability it fixes stays
unpatched for exactly as long as nobody notices, which, for a truly orphaned
repo, is indefinitely. Waiting for a review that will never come **is the
failure mode this project is trying to solve**, not a safer alternative to
solving it.

## Decision

When `internal/orphan.IsOrphaned` reports a repository as orphaned (no human
commit and no human PR review within the configured threshold, e.g. 180
days), `internal/vcs.GitHubVCS.SubmitPatch` merges the patch PR immediately
instead of leaving it open for review, provided — and only provided — the
patch has already passed the sandboxed test run in `internal/sandbox`.

## Risk analysis

Merging code without human review is a real risk, and this ADR does not
pretend otherwise. Three specific things could go wrong:

1. The sandboxed test suite could pass while the patch still breaks
   something in production the tests don't cover.
2. The dependency bump itself could be based on a wrong `UpdateCandidate`
   (e.g. targeting the wrong version).
3. Something about the orphan determination itself could be wrong — a repo
   could be misclassified as orphaned when in fact a human is still
   quietly relying on it, just not committing to it.

None of these risks are eliminated by requiring human review on a repo
where no human review is actually going to happen — in that world the
"review gate" isn't a safety mechanism, it's a permanent blocker with a
false appearance of safety. So the right question isn't "is auto-merge
risk-free," it's "does auto-merge plus Groundskeeper's other mechanisms
produce a better expected outcome than the realistic alternative (the patch
never landing at all)." It does, because of the safety nets below — none of
which depend on a human being available:

- **Sandboxed test run first (`internal/sandbox`).** The patch never lands
  without first proving, in an ephemeral, isolated container, that the
  repo's *own* test suite passes against it. A repo with no test suite gets
  `sandbox.ErrNoTestSuite` and Groundskeeper refuses to proceed at all —
  addressing risk (1) as much as an automated system reasonably can, and
  making "no tests, no auto-merge" a hard rule rather than a
  recommendation.
- **Post-merge health monitoring (`internal/monitor`).** Groundskeeper
  doesn't stop caring the moment the patch merges. It watches the target
  job's next several real runs and treats any failure as evidence the patch
  was actually bad, regardless of what the sandbox tests said beforehand.
  This is the mechanism that catches exactly the class of bug the sandbox
  can't: something that only manifests against real production data,
  timing, or environment.
- **Automatic rollback (`internal/state.Rollback` + `internal/vcs.
  SubmitRevert`).** The instant a monitored run fails, Groundskeeper reverts
  to the last-known-good commit it recorded before the patch, and does so
  through the same VCS path (a real revert PR, merged immediately) so the
  repository's history stays a legible, auditable record of what happened
  and why — not a silent, undocumented force-push.
- **Human notification (`internal/notify`).** Every automatic rollback
  fires a notification to a configured fallback contact. Auto-merge without
  review is only acceptable because it is never allowed to be the last word
  — a human is always told the moment something needed their attention,
  just not required to be the bottleneck for the routine case where nothing
  goes wrong.

Together these mean: a bad patch either never merges (caught by tests), or
merges and gets caught and reverted within a small number of production
runs (caught by monitoring), with a human told about it either way. The
uncaught, permanent-damage scenario Groundskeeper is trying to avoid — the
repo silently sitting on a fixed advisory forever, or a bad patch running
forever with nobody watching — is the one that auto-merge plus this safety
net actually prevents, and the one that "always require review" quietly
guarantees on a truly orphaned repo.

## Consequences

- `internal/orphan.IsOrphaned`'s threshold becomes an operationally
  significant parameter, not just a documentation detail — set it too low
  and Groundskeeper auto-merges on repos that still have an inattentive but
  present maintainer; set it too high and orphaned repos sit vulnerable
  longer than necessary. 180 days is this project's starting default, not
  a universal constant.
- Every auto-merge and every automatic rollback must be fully auditable
  after the fact: the PR/revert-PR history in `internal/vcs`, the state
  transitions in `internal/state`, and the notification in
  `internal/notify` together need to reconstruct exactly what happened,
  since no human approved any of it in the moment.
- If a repo's orphan status is ever wrong (a human actually is still
  around), that human will see PRs auto-merging into their repo without
  their input. `internal/notify` firing on rollback is a partial mitigation
  but not a full one; a production deployment would want the orphan
  determination itself to be conservative and easy for a returning
  maintainer to override.
