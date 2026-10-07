# ADR 0001: v1 targets Python (`requirements.txt`) end-to-end only

## Status

Accepted.

## Context

Groundskeeper's value proposition is a complete, trustworthy pipeline: watch
a manifest, patch it in a sandbox, prove the patch works via the repo's own
tests, decide whether a human is around to review it, and — crucially —
watch production afterward and roll back automatically if something the
sandbox missed goes wrong. Every stage of that pipeline depends on
ecosystem-specific details:

- **Watcher** needs to parse the manifest format and know the right OSV
  ecosystem identifier (`PyPI`, `npm`, `Go`, ...) and registry API (PyPI's
  JSON API, npm's registry API, the Go module proxy) to check for newer
  versions.
- **Sandbox** needs to know the right base image, install command (`pip
  install -r requirements.txt` vs. `npm ci` vs. `go mod download`), and how
  to invoke the repo's test suite.
- **VCS** and **state** are, encouragingly, already ecosystem-agnostic: a PR
  is a PR, a commit SHA is a commit SHA, regardless of what manifest format
  changed.

Building shallow support for three ecosystems at once — Python, Node, and
Go — would mean the demo, the tests, and the documentation all have to
divide their attention three ways, and would make it much harder to get any
single path fully working end to end, with a fixture that's genuinely
functional and a test suite that actually demonstrates the safety guarantees
this project is about.

## Decision

v1 supports **Python repositories using `requirements.txt` only**, built out
completely: manifest parsing, OSV + PyPI querying, sandboxed patch + test
execution, orphan detection, auto-merge, post-merge monitoring, and
automatic rollback all work end-to-end for this one case, backed by a real
functional demo fixture (`testdata/example-orphaned-script/`).

Node (`package.json` / `package-lock.json` / npm registry / OSV ecosystem
`npm`) and Go (`go.mod` / `go.sum` / the Go module proxy / OSV ecosystem
`Go`) are named, intended future adapters, **not built in this version**.

## Why this is the right boundary

The ecosystem-specific logic is already isolated behind two interfaces:

- `internal/watcher` parses a manifest into `PinnedDependency` values and
  emits `UpdateCandidate` values — a Node or Go watcher would implement the
  same `PinnedDependency`-in, `UpdateCandidate`-out contract against a
  different manifest format and registry API, without changing anything
  downstream.
- `internal/sandbox.RepoConfig` already parameterizes the base image, the
  install command, and the test command — a Node adapter is "different
  defaults for those three fields plus different `package.json` line-editing
  logic in place of `ApplyCandidateToRequirements`," not a different
  pipeline.

Because both of those seams already exist, scoping v1 to Python doesn't
create throwaway work later: adding Node or Go support means writing a new
implementation behind an existing interface, not restructuring the
pipeline. The Watcher and Sandbox packages' doc comments and the README's
architecture table both call this out explicitly so it isn't a surprise to
anyone reading the code.

## Consequences

- The demo fixture, the CLI, and the test suite only need to prove out one
  ecosystem, which means they can be complete and genuinely functional-
  looking rather than three shallow, unconvincing sketches.
- A real production deployment covering a mixed-language fleet of orphaned
  scripts would need Node/Go adapters built before Groundskeeper could help
  with those repos — this is a real, acknowledged limitation of v1, not a
  hidden one.
- `internal/vcs` and `internal/state`, being already ecosystem-agnostic,
  need no changes at all when a second ecosystem adapter is added later.
