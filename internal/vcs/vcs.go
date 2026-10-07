// Package vcs integrates Groundskeeper with GitHub: opening a normal pull
// request for a patch on a repo that still has active human maintainers, or
// merging directly when internal/orphan has determined nobody is left to
// review it. It also handles opening/merging the revert when
// internal/monitor detects a bad patch after the fact.
//
// The VCS interface exists so the rest of Groundskeeper (the CLI, the
// patch-pipeline orchestration) never depends on go-github directly, which
// keeps it swappable (e.g. for GitLab, or for a fake in tests).
package vcs

import (
	"context"
	"fmt"

	"github.com/google/go-github/v66/github"

	"github.com/mstone/groundskeeper/internal/watcher"
)

// RepoRef identifies a single GitHub repository.
type RepoRef struct {
	Owner string
	Name  string
}

func (r RepoRef) String() string {
	return fmt.Sprintf("%s/%s", r.Owner, r.Name)
}

// PatchSubmission describes a completed, sandbox-tested patch ready to land,
// either as a PR or as a direct merge.
type PatchSubmission struct {
	Repo RepoRef

	// BaseBranch is the branch the patch should land on (typically the
	// repository's default branch).
	BaseBranch string

	// HeadBranch is the branch the patch commit(s) were already pushed to
	// before calling into this package; vcs itself does not push commits,
	// it only opens/merges PRs against branches that already exist.
	HeadBranch string

	Candidate watcher.UpdateCandidate

	// SandboxLogsURL optionally links to where the sandbox test run's full
	// logs can be viewed (e.g. a CI artifact URL), included in the PR body
	// for auditability.
	SandboxLogsURL string
}

// RevertSubmission describes a rollback that needs to be landed after
// internal/monitor observed a broken run.
type RevertSubmission struct {
	Repo RepoRef

	BaseBranch string
	HeadBranch string

	// RevertedCandidate is the patch being reverted.
	RevertedCandidate watcher.UpdateCandidate

	// Reason is a short human-readable explanation (e.g. "run failed with
	// exit code 1 three consecutive times"), included in the PR/commit
	// body.
	Reason string
}

// LandedChange describes the result of successfully landing a patch or
// revert, regardless of whether that happened via direct merge or via a
// reviewed PR.
type LandedChange struct {
	// MergedDirectly is true when the change was merged immediately
	// without a review (because the repo was orphaned), false when it was
	// opened as a PR awaiting review.
	MergedDirectly bool

	// PullRequestNumber is set when a PR was opened (whether or not it was
	// then immediately merged).
	PullRequestNumber int

	// MergeCommitSHA is set once the change has actually landed on
	// BaseBranch, whether via direct merge or PR merge.
	MergeCommitSHA string
}

// VCS is the interface the rest of Groundskeeper depends on for landing
// patches and reverts. GitHubVCS is the real implementation; tests and the
// CLI's dry-run mode can substitute a fake.
type VCS interface {
	// SubmitPatch opens a PR for sub. If orphaned is true, it also merges
	// that PR immediately instead of leaving it for human review, since
	// review on an orphaned repo would never come. See
	// docs/adr/0002-auto-merge-for-orphaned-repos.md.
	SubmitPatch(ctx context.Context, sub PatchSubmission, orphaned bool) (*LandedChange, error)

	// SubmitRevert opens a PR reverting a previously landed patch and
	// always merges it immediately: a revert triggered by
	// internal/monitor is, by definition, responding to an already-broken
	// production run, so waiting for review would only prolong the
	// outage.
	SubmitRevert(ctx context.Context, sub RevertSubmission) (*LandedChange, error)
}

// GitHubVCS implements VCS against the real GitHub REST API via go-github.
type GitHubVCS struct {
	Client *github.Client
}

// NewGitHubVCS wraps an authenticated *github.Client.
func NewGitHubVCS(client *github.Client) *GitHubVCS {
	return &GitHubVCS{Client: client}
}

// SubmitPatch opens a pull request for sub and, when orphaned is true,
// merges it immediately.
//
// This performs real GitHub API calls when invoked; it is never invoked as
// part of this project's scaffolding. It requires Client to be authenticated
// with a token that has write access to sub.Repo.
func (v *GitHubVCS) SubmitPatch(ctx context.Context, sub PatchSubmission, orphaned bool) (*LandedChange, error) {
	title := patchTitle(sub.Candidate)
	body := patchBody(sub)

	pr, _, err := v.Client.PullRequests.Create(ctx, sub.Repo.Owner, sub.Repo.Name, &github.NewPullRequest{
		Title: github.String(title),
		Head:  github.String(sub.HeadBranch),
		Base:  github.String(sub.BaseBranch),
		Body:  github.String(body),
	})
	if err != nil {
		return nil, fmt.Errorf("vcs: opening PR for %s on %s: %w", sub.Candidate.Package, sub.Repo, err)
	}

	if !orphaned {
		// A human maintainer is still around; leave the PR for their
		// review instead of merging on their behalf.
		return &LandedChange{
			MergedDirectly:    false,
			PullRequestNumber: pr.GetNumber(),
		}, nil
	}

	mergeMsg := fmt.Sprintf(
		"Auto-merged by Groundskeeper: %s has had no human commit or PR review activity within the "+
			"orphan threshold, so this sandboxed, test-passing security patch was merged directly "+
			"rather than left waiting for a review that would never come.",
		sub.Repo,
	)
	result, _, err := v.Client.PullRequests.Merge(ctx, sub.Repo.Owner, sub.Repo.Name, pr.GetNumber(), mergeMsg, &github.PullRequestOptions{
		MergeMethod: "squash",
	})
	if err != nil {
		return nil, fmt.Errorf("vcs: auto-merging PR #%d for %s on %s: %w", pr.GetNumber(), sub.Candidate.Package, sub.Repo, err)
	}

	return &LandedChange{
		MergedDirectly:    true,
		PullRequestNumber: pr.GetNumber(),
		MergeCommitSHA:    result.GetSHA(),
	}, nil
}

// SubmitRevert opens and immediately merges a revert PR for sub.
//
// This performs real GitHub API calls when invoked; it is never invoked as
// part of this project's scaffolding.
func (v *GitHubVCS) SubmitRevert(ctx context.Context, sub RevertSubmission) (*LandedChange, error) {
	title := fmt.Sprintf("Revert: %s", patchTitle(sub.RevertedCandidate))
	body := revertBody(sub)

	pr, _, err := v.Client.PullRequests.Create(ctx, sub.Repo.Owner, sub.Repo.Name, &github.NewPullRequest{
		Title: github.String(title),
		Head:  github.String(sub.HeadBranch),
		Base:  github.String(sub.BaseBranch),
		Body:  github.String(body),
	})
	if err != nil {
		return nil, fmt.Errorf("vcs: opening revert PR for %s on %s: %w", sub.RevertedCandidate.Package, sub.Repo, err)
	}

	mergeMsg := fmt.Sprintf(
		"Auto-reverted by Groundskeeper: %s. Notifying the configured fallback contact.",
		sub.Reason,
	)
	result, _, err := v.Client.PullRequests.Merge(ctx, sub.Repo.Owner, sub.Repo.Name, pr.GetNumber(), mergeMsg, &github.PullRequestOptions{
		MergeMethod: "squash",
	})
	if err != nil {
		return nil, fmt.Errorf("vcs: merging revert PR #%d for %s on %s: %w", pr.GetNumber(), sub.RevertedCandidate.Package, sub.Repo, err)
	}

	return &LandedChange{
		MergedDirectly:    true,
		PullRequestNumber: pr.GetNumber(),
		MergeCommitSHA:    result.GetSHA(),
	}, nil
}

func patchTitle(c watcher.UpdateCandidate) string {
	kind := "update"
	if c.IsSecurityFix {
		kind = "security fix"
	}
	title := fmt.Sprintf("groundskeeper: bump %s %s -> %s (%s)", c.Package, c.CurrentVersion, c.TargetVersion, kind)
	if c.AdvisoryID != "" {
		title += fmt.Sprintf(" [%s]", c.AdvisoryID)
	}
	return title
}

func patchBody(sub PatchSubmission) string {
	body := fmt.Sprintf(
		"Groundskeeper detected that `%s` is pinned at `%s` and should be bumped to `%s`.\n\n",
		sub.Candidate.Package, sub.Candidate.CurrentVersion, sub.Candidate.TargetVersion,
	)
	if sub.Candidate.IsSecurityFix {
		body += fmt.Sprintf("This addresses security advisory `%s`.\n\n", sub.Candidate.AdvisoryID)
	}
	body += "This patch was applied and its test suite run inside an ephemeral sandbox container; all tests passed.\n"
	if sub.SandboxLogsURL != "" {
		body += fmt.Sprintf("Full sandbox logs: %s\n", sub.SandboxLogsURL)
	}
	body += "\nIf this repository is flagged as orphaned, Groundskeeper will merge this PR immediately " +
		"and continue watching the next several real runs, automatically rolling back if anything the " +
		"sandbox tests didn't catch turns out to be broken."
	return body
}

func revertBody(sub RevertSubmission) string {
	return fmt.Sprintf(
		"Groundskeeper is reverting the previously auto-merged bump of `%s` to `%s`.\n\n"+
			"Reason: %s\n\n"+
			"The fallback contact configured for this repository has been notified.",
		sub.RevertedCandidate.Package, sub.RevertedCandidate.TargetVersion, sub.Reason,
	)
}
