// Package sandbox runs a candidate dependency patch against a target repo's
// own test suite inside an ephemeral, disposable Docker container, so an
// untrusted or unreviewed patch can never touch anything but a throwaway
// environment before Groundskeeper decides whether to merge it.
//
// Nothing in this package is invoked as part of scaffolding this project:
// the Docker Engine API calls are real, structured client code (using
// github.com/docker/docker/client) but require a live Docker daemon and are
// only ever exercised from the `groundskeeper patch` CLI subcommand or from
// the watcher's auto-patch pipeline in a real deployment.
package sandbox

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"

	"github.com/mstone/groundskeeper/internal/watcher"
)

// ErrNoTestSuite is returned by Run when the target repo has no configured
// test command. Groundskeeper refuses to auto-merge a patch it cannot prove
// passes tests, so this is a hard stop rather than a silent no-op.
var ErrNoTestSuite = errors.New("sandbox: target repo has no configured test command; refusing to patch blind")

const (
	// defaultPythonImage is the base image ephemeral patch containers are
	// built from. Pinning to a digest (rather than a floating tag) would be
	// the production choice; a tag is used here for readability.
	defaultPythonImage = "python:3.11-slim"

	// containerWorkdir is where the target repo checkout is copied to
	// inside the sandbox container.
	containerWorkdir = "/workspace"

	requirementsFilename = "requirements.txt"
)

// RepoConfig describes the target repository being patched: where its
// checkout lives on the host, and how to install and test it inside the
// sandbox container.
type RepoConfig struct {
	// HostPath is the path to a fresh checkout of the target repo on the
	// machine running Groundskeeper (e.g. a temp clone made just for this
	// patch attempt).
	HostPath string

	// TestCommand is the shell command used to run the repo's own test
	// suite inside the container, e.g. []string{"pytest", "-q"}. Empty
	// means the repo has no configured tests, in which case Run returns
	// ErrNoTestSuite instead of proceeding.
	TestCommand []string

	// InstallCommand is the shell command used to install dependencies
	// from the (patched) requirements.txt before running tests. Defaults
	// to []string{"pip", "install", "-r", "requirements.txt"} if empty.
	InstallCommand []string

	// Image overrides the base image used for the ephemeral container.
	// Defaults to defaultPythonImage if empty.
	Image string
}

// PatchAttemptResult captures the full outcome of trying one UpdateCandidate
// inside the sandbox: whether tests passed, the combined log output, and how
// long the attempt took.
type PatchAttemptResult struct {
	Candidate watcher.UpdateCandidate
	Passed    bool
	Logs      string
	Duration  time.Duration
	ExitCode  int
}

// Sandbox runs patch attempts inside ephemeral Docker containers.
type Sandbox struct {
	// Docker is the Engine API client used to create/start/remove
	// containers. Callers typically construct it with
	// client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation()).
	Docker *client.Client

	// Timeout bounds the whole patch attempt (image pull, container
	// creation, dependency install, and test run). Defaults to 5 minutes
	// if zero.
	Timeout time.Duration
}

// New constructs a Sandbox from an existing Docker Engine API client.
func New(docker *client.Client) *Sandbox {
	return &Sandbox{Docker: docker, Timeout: 5 * time.Minute}
}

// Run applies candidate to cfg's requirements.txt, spins up an ephemeral
// container from cfg's checkout, installs dependencies, and runs the repo's
// test command, returning a PatchAttemptResult describing what happened.
//
// Run never mutates cfg.HostPath's requirements.txt in place for the
// "real" patch application — see ApplyCandidateToRequirements below for the
// pure, independently-testable line-editing logic used to produce the
// patched manifest bytes that get written into the container.
func (s *Sandbox) Run(ctx context.Context, cfg RepoConfig, candidate watcher.UpdateCandidate) (*PatchAttemptResult, error) {
	if len(cfg.TestCommand) == 0 {
		return nil, ErrNoTestSuite
	}
	if s.Docker == nil {
		return nil, errors.New("sandbox: no Docker client configured")
	}

	timeout := s.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()

	imageName := cfg.Image
	if imageName == "" {
		imageName = defaultPythonImage
	}
	installCmd := cfg.InstallCommand
	if len(installCmd) == 0 {
		installCmd = []string{"pip", "install", "-r", requirementsFilename}
	}

	// 1. Pull the base image (idempotent if already cached locally).
	pullReader, err := s.Docker.ImagePull(ctx, imageName, image.PullOptions{})
	if err != nil {
		return nil, fmt.Errorf("sandbox: pulling image %s: %w", imageName, err)
	}
	// Drain and discard pull progress output; a real implementation might
	// stream this to a logger.
	if _, err := io.Copy(io.Discard, pullReader); err != nil {
		pullReader.Close()
		return nil, fmt.Errorf("sandbox: reading image pull progress: %w", err)
	}
	pullReader.Close()

	// 2. Create the ephemeral container. It runs an indefinite sleep so we
	// can exec install/test commands into it, then gets torn down
	// unconditionally via the deferred remove below.
	created, err := s.Docker.ContainerCreate(ctx,
		&container.Config{
			Image:      imageName,
			Cmd:        []string{"sleep", "infinity"},
			WorkingDir: containerWorkdir,
			Tty:        false,
			Labels: map[string]string{
				"groundskeeper.managed":   "true",
				"groundskeeper.package":   candidate.Package,
				"groundskeeper.candidate": candidate.TargetVersion,
			},
		},
		&container.HostConfig{
			AutoRemove: false, // removed explicitly below so logs can be captured first
			// A production deployment would additionally set resource
			// limits (Memory, NanoCPUs), drop capabilities, disable
			// networking except to PyPI, and run as a non-root user.
		},
		nil, nil, "",
	)
	if err != nil {
		return nil, fmt.Errorf("sandbox: creating container: %w", err)
	}
	containerID := created.ID

	defer func() {
		// Best-effort cleanup; the ephemeral container must never outlive
		// the patch attempt regardless of success or failure.
		_ = s.Docker.ContainerRemove(context.Background(), containerID, container.RemoveOptions{
			Force: true,
		})
	}()

	if err := s.Docker.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("sandbox: starting container: %w", err)
	}

	// 3. Copy in a fresh checkout of the target repo, with the manifest
	// patched in-flight (the on-disk checkout at cfg.HostPath is never
	// mutated; only the tar stream sent into the container reflects the
	// candidate version bump).
	patchedTar, err := buildPatchedRepoTar(cfg.HostPath, candidate)
	if err != nil {
		return nil, fmt.Errorf("sandbox: preparing patched repo archive: %w", err)
	}
	if err := s.Docker.CopyToContainer(ctx, containerID, containerWorkdir, patchedTar, container.CopyToContainerOptions{}); err != nil {
		return nil, fmt.Errorf("sandbox: copying repo into container: %w", err)
	}

	var logs bytes.Buffer

	// 4. Install dependencies from the patched requirements.txt.
	installExitCode, err := execInContainer(ctx, s.Docker, containerID, installCmd, &logs)
	if err != nil {
		return nil, fmt.Errorf("sandbox: running install command: %w", err)
	}
	if installExitCode != 0 {
		return &PatchAttemptResult{
			Candidate: candidate,
			Passed:    false,
			Logs:      logs.String(),
			Duration:  time.Since(start),
			ExitCode:  installExitCode,
		}, nil
	}

	// 5. Run the repo's own test suite.
	testExitCode, err := execInContainer(ctx, s.Docker, containerID, cfg.TestCommand, &logs)
	if err != nil {
		return nil, fmt.Errorf("sandbox: running test command: %w", err)
	}

	return &PatchAttemptResult{
		Candidate: candidate,
		Passed:    testExitCode == 0,
		Logs:      logs.String(),
		Duration:  time.Since(start),
		ExitCode:  testExitCode,
	}, nil
}

// execInContainer runs cmd inside containerID via the Docker exec API,
// appending combined stdout/stderr to out, and returns the process's exit
// code.
func execInContainer(ctx context.Context, docker *client.Client, containerID string, cmd []string, out *bytes.Buffer) (int, error) {
	execCreated, err := docker.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return -1, fmt.Errorf("creating exec for %q: %w", strings.Join(cmd, " "), err)
	}

	attached, err := docker.ContainerExecAttach(ctx, execCreated.ID, container.ExecAttachOptions{})
	if err != nil {
		return -1, fmt.Errorf("attaching to exec for %q: %w", strings.Join(cmd, " "), err)
	}
	defer attached.Close()

	fmt.Fprintf(out, "$ %s\n", strings.Join(cmd, " "))
	if _, err := io.Copy(out, attached.Reader); err != nil {
		return -1, fmt.Errorf("reading exec output for %q: %w", strings.Join(cmd, " "), err)
	}

	inspected, err := docker.ContainerExecInspect(ctx, execCreated.ID)
	if err != nil {
		return -1, fmt.Errorf("inspecting exec result for %q: %w", strings.Join(cmd, " "), err)
	}
	return inspected.ExitCode, nil
}

// buildPatchedRepoTar walks hostPath and produces an in-memory tar stream
// suitable for CopyToContainer, rewriting requirements.txt on the fly to
// reflect candidate.
func buildPatchedRepoTar(hostPath string, candidate watcher.UpdateCandidate) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	err := filepath.Walk(hostPath, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relPath, err := filepath.Rel(hostPath, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}
		// Skip VCS metadata; it has no place inside the sandbox container.
		if strings.HasPrefix(relPath, ".git") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if info.IsDir() {
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = filepath.ToSlash(relPath) + "/"
			return tw.WriteHeader(hdr)
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if filepath.Base(relPath) == requirementsFilename {
			content = ApplyCandidateToRequirements(content, candidate)
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(relPath)
		hdr.Size = int64(len(content))
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = tw.Write(content)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}

// ApplyCandidateToRequirements rewrites the pinned version for
// candidate.Package inside a requirements.txt's raw contents, leaving every
// other line (including comments, blank lines, and unrelated pins) byte-for-
// byte untouched. If the package isn't found, the content is returned
// unmodified.
//
// This is pure, dependency-free line editing so it can be unit tested
// without Docker or a filesystem.
func ApplyCandidateToRequirements(content []byte, candidate watcher.UpdateCandidate) []byte {
	lines := strings.Split(string(content), "\n")
	targetName := strings.ToLower(strings.TrimSpace(candidate.Package))

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		body := trimmed
		var trailingComment string
		if idx := strings.Index(body, "#"); idx >= 0 {
			trailingComment = body[idx:]
			body = strings.TrimSpace(body[:idx])
		}
		const pinOp = "=="
		eqIdx := strings.Index(body, pinOp)
		if eqIdx <= 0 {
			continue
		}
		name := strings.TrimSpace(body[:eqIdx])
		if !strings.EqualFold(name, targetName) {
			continue
		}

		versionAndMarker := strings.TrimSpace(body[eqIdx+len(pinOp):])
		marker := ""
		if semiIdx := strings.Index(versionAndMarker, ";"); semiIdx >= 0 {
			marker = versionAndMarker[semiIdx:]
		}

		newLine := name + pinOp + candidate.TargetVersion
		if marker != "" {
			newLine += " " + marker
		}
		if trailingComment != "" {
			newLine += "  " + trailingComment
		}
		lines[i] = newLine
	}

	return []byte(strings.Join(lines, "\n"))
}

// bufferedExecReader is a small helper kept for parity with docker's
// streaming multiplexed output when TTY is disabled; retained here as a
// documented seam in case stdout/stderr ever need to be demultiplexed
// separately (Docker multiplexes them with an 8-byte frame header per
// https://pkg.go.dev/github.com/docker/docker/client#Client.ContainerAttach
// when Tty is false). Not currently used since ContainerExecAttach's raw
// reader is copied wholesale into the combined log buffer above.
var _ = bufio.NewReader
