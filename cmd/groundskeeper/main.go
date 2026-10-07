// Command groundskeeper is the CLI entry point for Groundskeeper: a tool
// that watches a small, unmaintained script's pinned Python dependencies for
// security advisories, patches them inside a sandbox, and auto-merges the
// fix (with a post-merge safety net) when nobody is left to review it.
//
// Subcommands:
//
//	groundskeeper watch  --repo <path>
//	groundskeeper patch  --repo <path> --candidate <pkg@version>
//	groundskeeper status --repo <path>
//
// None of these subcommands are exercised as part of scaffolding this
// project — this file wires the internal packages together but performs no
// build, run, or network I/O until a real deployment invokes the compiled
// binary.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mstone/groundskeeper/internal/state"
	"github.com/mstone/groundskeeper/internal/watcher"
)

// stateFileName is the name of the per-repo Groundskeeper bookkeeping file,
// written inside the target repo's own directory (and ignored via
// .gitignore's "*.groundskeeper-state.json" entry so it never accidentally
// gets committed to the target repo itself).
const stateFileName = ".groundskeeper-state.json"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "watch":
		err = runWatch(os.Args[2:])
	case "patch":
		err = runPatch(os.Args[2:])
	case "status":
		err = runStatus(os.Args[2:])
	case "-h", "--help", "help":
		printUsage()
		return
	default:
		fmt.Fprintf(os.Stderr, "groundskeeper: unknown subcommand %q\n\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "groundskeeper: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `groundskeeper: keeps an orphaned script's dependencies patched and safe.

Usage:

  groundskeeper watch  --repo <path>
      Check the target repo's requirements.txt against OSV and PyPI and
      print a prioritized list of update candidates (security fixes first).

  groundskeeper patch  --repo <path> --candidate <pkg@version>
      Apply a single update candidate inside a sandboxed container, run the
      repo's test suite, and (on success) record it as the active patch in
      the repo's maintenance state file. Fails clearly if the repo has no
      configured test command (see sandbox.ErrNoTestSuite).

  groundskeeper status --repo <path>
      Print the current MaintenanceState recorded for the target repo.

`)
}

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	repo := fs.String("repo", "", "path to the target repository")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repo == "" {
		return fmt.Errorf("watch: --repo is required")
	}

	reqPath := filepath.Join(*repo, "requirements.txt")
	deps, err := watcher.ParseRequirementsFile(reqPath)
	if err != nil {
		return fmt.Errorf("watch: %w", err)
	}
	if len(deps) == 0 {
		fmt.Printf("groundskeeper: no exact-pinned dependencies found in %s\n", reqPath)
		return nil
	}

	w := watcher.New()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	candidates, err := w.Check(ctx, deps)
	if err != nil {
		return fmt.Errorf("watch: checking dependencies: %w", err)
	}

	if len(candidates) == 0 {
		fmt.Println("groundskeeper: all pinned dependencies are up to date and have no known advisories")
		return nil
	}

	fmt.Printf("groundskeeper: found %d update candidate(s) for %s\n\n", len(candidates), *repo)
	for _, c := range candidates {
		if c.IsSecurityFix {
			fmt.Printf("  [SECURITY %s] %s %s -> %s\n", c.AdvisoryID, c.Package, c.CurrentVersion, c.TargetVersion)
		} else {
			fmt.Printf("  [update]           %s %s -> %s\n", c.Package, c.CurrentVersion, c.TargetVersion)
		}
	}
	return nil
}

func runPatch(args []string) error {
	fs := flag.NewFlagSet("patch", flag.ExitOnError)
	repo := fs.String("repo", "", "path to the target repository")
	candidateFlag := fs.String("candidate", "", "the update candidate to apply, as pkg@version (e.g. requests@2.31.0)")
	testCmd := fs.String("test-cmd", "pytest -q", "the test command to run inside the sandbox, space-separated")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repo == "" {
		return fmt.Errorf("patch: --repo is required")
	}
	if *candidateFlag == "" {
		return fmt.Errorf("patch: --candidate is required, e.g. --candidate requests@2.31.0")
	}

	pkg, targetVersion, err := parsePkgAtVersion(*candidateFlag)
	if err != nil {
		return fmt.Errorf("patch: %w", err)
	}

	reqPath := filepath.Join(*repo, "requirements.txt")
	deps, err := watcher.ParseRequirementsFile(reqPath)
	if err != nil {
		return fmt.Errorf("patch: %w", err)
	}
	currentVersion := ""
	for _, d := range deps {
		if strings.EqualFold(d.Package, pkg) {
			currentVersion = d.Version
			break
		}
	}
	if currentVersion == "" {
		return fmt.Errorf("patch: %s is not an exact-pinned dependency in %s", pkg, reqPath)
	}

	candidate := watcher.UpdateCandidate{
		Package:        pkg,
		CurrentVersion: currentVersion,
		TargetVersion:  targetVersion,
	}

	// NOTE: constructing a real *sandbox.Sandbox requires a live Docker
	// daemon (via client.NewClientWithOpts(client.FromEnv,
	// client.WithAPIVersionNegotiation())) and is intentionally left as a
	// wiring point rather than invoked here, consistent with this
	// project's "structured but never executed" scaffolding constraint.
	// A real deployment would do roughly:
	//
	//   dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	//   sb := sandbox.New(dockerClient)
	//   result, err := sb.Run(ctx, sandbox.RepoConfig{
	//       HostPath:    *repo,
	//       TestCommand: strings.Fields(*testCmd),
	//   }, candidate)
	//
	// and then, only if result.Passed, proceed to record the patch via
	// state.ApplyPatch and land it via internal/vcs.
	fmt.Printf(
		"groundskeeper: would sandbox-test %s %s -> %s in %s using test command %q, "+
			"then (on success) record it via state.ApplyPatch and land it via internal/vcs.\n",
		candidate.Package, candidate.CurrentVersion, candidate.TargetVersion, *repo, *testCmd,
	)
	fmt.Println("groundskeeper: no Docker daemon was contacted; this is a dry-run wiring preview only.")
	return nil
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	repo := fs.String("repo", "", "path to the target repository")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repo == "" {
		return fmt.Errorf("status: --repo is required")
	}

	statePath := filepath.Join(*repo, stateFileName)
	s, err := state.Load(statePath)
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}

	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("status: formatting state: %w", err)
	}
	fmt.Println(string(out))
	return nil
}

// parsePkgAtVersion splits a "pkg@version" flag value into its parts.
func parsePkgAtVersion(s string) (pkg, version string, err error) {
	idx := strings.LastIndex(s, "@")
	if idx <= 0 || idx == len(s)-1 {
		return "", "", fmt.Errorf("invalid --candidate %q, expected format pkg@version", s)
	}
	return s[:idx], s[idx+1:], nil
}
