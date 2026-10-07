// Package watcher scans a target repository's Python dependency manifest
// (requirements.txt) and produces a prioritized list of update candidates by
// cross-referencing each pinned package/version against:
//
//   - the OSV.dev vulnerability database (https://osv.dev), to find known
//     security advisories affecting the pinned version, and
//   - the PyPI JSON API, to find newer versions available upstream.
//
// Nothing in this package is wired up to run automatically; the HTTP calls
// are real, structured client code but are only ever invoked when a caller
// (e.g. the `groundskeeper watch` CLI subcommand) explicitly calls Check.
package watcher

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	// osvQueryURL is the OSV.dev batch-free single query endpoint.
	// See https://osv.dev/docs/#tag/api/operation/OSV_QueryAffected
	osvQueryURL = "https://api.osv.dev/v1/query"

	// pypiJSONURLFormat is PyPI's per-project JSON metadata endpoint.
	// See https://warehouse.pypa.io/api-reference/json.html
	pypiJSONURLFormat = "https://pypi.org/pypi/%s/json"

	// pypiEcosystem is the OSV ecosystem identifier for PyPI packages.
	// See https://ossf.github.io/osv-schema/#affectedpackage-field
	pypiEcosystem = "PyPI"

	defaultHTTPTimeout = 15 * time.Second
)

// PinnedDependency is a single `name==version` line parsed out of a
// requirements.txt file.
type PinnedDependency struct {
	Package string
	Version string
}

// UpdateCandidate describes a single package that Groundskeeper believes
// should be bumped, and why.
type UpdateCandidate struct {
	Package        string `json:"package"`
	CurrentVersion string `json:"current_version"`
	TargetVersion  string `json:"target_version"`
	IsSecurityFix  bool   `json:"is_security_fix"`
	// AdvisoryID is the OSV advisory identifier (e.g. "GHSA-xxxx-xxxx-xxxx"
	// or "PYSEC-2023-1234") that motivated this candidate. Empty when
	// IsSecurityFix is false and the candidate exists only because a newer
	// non-security release is available.
	AdvisoryID string `json:"advisory_id,omitempty"`
}

// --- OSV API request/response shapes -------------------------------------
//
// These mirror the real request/response bodies documented at
// https://osv.dev/docs/#tag/api/operation/OSV_QueryAffected. They are never
// unmarshalled against live traffic in this scaffold, but are written to the
// real shape so the HTTP call below is a drop-in once network access is
// wired up in an integration environment.

// osvPackage identifies a package within an OSV query.
type osvPackage struct {
	Name      string `json:"name"`
	Ecosystem string `json:"ecosystem"`
}

// osvQueryRequest is the POST body sent to api.osv.dev/v1/query.
type osvQueryRequest struct {
	Version string     `json:"version,omitempty"`
	Package osvPackage `json:"package"`
}

// osvSeverity is one severity scoring entry attached to a vulnerability.
type osvSeverity struct {
	Type  string `json:"type"`
	Score string `json:"score"`
}

// osvVulnerability is a single advisory returned by OSV.
type osvVulnerability struct {
	ID       string        `json:"id"`
	Summary  string        `json:"summary"`
	Details  string        `json:"details"`
	Aliases  []string      `json:"aliases"`
	Severity []osvSeverity `json:"severity"`
	Affected []struct {
		Package osvPackage `json:"package"`
		Ranges  []struct {
			Type   string `json:"type"`
			Events []struct {
				Introduced string `json:"introduced,omitempty"`
				Fixed      string `json:"fixed,omitempty"`
			} `json:"events"`
		} `json:"ranges"`
	} `json:"affected"`
}

// osvQueryResponse is the JSON body OSV returns for a query request.
type osvQueryResponse struct {
	Vulns []osvVulnerability `json:"vulns"`
}

// --- PyPI JSON API response shape -----------------------------------------

// pypiProjectResponse is the (trimmed) shape of PyPI's per-project JSON
// endpoint, https://pypi.org/pypi/<project>/json.
type pypiProjectResponse struct {
	Info struct {
		Name    string `json:"name"`
		Version string `json:"version"` // the latest release PyPI considers "current"
	} `json:"info"`
	Releases map[string][]struct {
		UploadTimeISO8601 string `json:"upload_time_iso_8601"`
		Yanked            bool   `json:"yanked"`
	} `json:"releases"`
}

// Watcher checks a target repository's pinned Python dependencies for
// security advisories and available upstream updates.
type Watcher struct {
	// HTTPClient is used for both OSV and PyPI calls. Defaults to a client
	// with a conservative timeout if nil.
	HTTPClient *http.Client
}

// New returns a Watcher configured with sane defaults.
func New() *Watcher {
	return &Watcher{
		HTTPClient: &http.Client{Timeout: defaultHTTPTimeout},
	}
}

// ParseRequirementsFile reads a requirements.txt file at path and returns
// each `pkg==version` pin found in it. Comments (#...), blank lines, and
// non-pinned lines (e.g. "-r other.txt", "pkg>=1.0", extras/markers without
// an exact pin) are skipped rather than erroring, since Groundskeeper can
// only safely reason about exact pins.
func ParseRequirementsFile(path string) ([]PinnedDependency, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("watcher: opening requirements file: %w", err)
	}
	defer f.Close()
	return ParseRequirements(f)
}

// ParseRequirements parses requirements.txt-formatted content from r.
func ParseRequirements(r io.Reader) ([]PinnedDependency, error) {
	var deps []PinnedDependency
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Strip inline comments, e.g. "requests==2.28.1  # pinned for CI".
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}
		// Skip non-pin directives like "-r base.txt" or "--index-url ...".
		if strings.HasPrefix(line, "-") {
			continue
		}
		// Only exact pins ("pkg==1.2.3") are actionable; ranges/extras are
		// left alone since Groundskeeper can't know which resolved version
		// is actually installed.
		const pinOp = "=="
		idx := strings.Index(line, pinOp)
		if idx <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:idx])
		version := strings.TrimSpace(line[idx+len(pinOp):])
		// Drop any environment marker suffix, e.g. "pkg==1.2.3; python_version>='3.8'".
		if semiIdx := strings.Index(version, ";"); semiIdx >= 0 {
			version = strings.TrimSpace(version[:semiIdx])
		}
		if name == "" || version == "" {
			continue
		}
		deps = append(deps, PinnedDependency{Package: name, Version: version})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("watcher: scanning requirements content: %w", err)
	}
	return deps, nil
}

// Check queries OSV and PyPI for every pinned dependency and returns the
// resulting update candidates, security fixes sorted first (and, within
// each group, alphabetically by package name for stable output).
//
// Check is never invoked as part of this scaffolding pass; it performs real
// outbound HTTP requests and is intended to be called from the `watch` CLI
// subcommand or a scheduled job in a real deployment.
func (w *Watcher) Check(ctx context.Context, deps []PinnedDependency) ([]UpdateCandidate, error) {
	client := w.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}

	var candidates []UpdateCandidate
	for _, dep := range deps {
		vulns, err := queryOSV(ctx, client, dep)
		if err != nil {
			return nil, fmt.Errorf("watcher: querying OSV for %s==%s: %w", dep.Package, dep.Version, err)
		}

		latest, err := latestPyPIVersion(ctx, client, dep.Package)
		if err != nil {
			return nil, fmt.Errorf("watcher: querying PyPI for %s: %w", dep.Package, err)
		}

		if len(vulns) > 0 {
			// A security advisory exists against the currently pinned
			// version. Target the latest known-good release; a more
			// sophisticated implementation would target the minimum
			// "fixed" version from the advisory's ranges instead of the
			// unconditional latest.
			for _, v := range vulns {
				candidates = append(candidates, UpdateCandidate{
					Package:        dep.Package,
					CurrentVersion: dep.Version,
					TargetVersion:  latest,
					IsSecurityFix:  true,
					AdvisoryID:     v.ID,
				})
			}
			continue
		}

		if latest != "" && latest != dep.Version {
			candidates = append(candidates, UpdateCandidate{
				Package:        dep.Package,
				CurrentVersion: dep.Version,
				TargetVersion:  latest,
				IsSecurityFix:  false,
			})
		}
	}

	sortCandidates(candidates)
	return candidates, nil
}

// sortCandidates orders security fixes first, then alphabetically by
// package name within each group.
func sortCandidates(candidates []UpdateCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].IsSecurityFix != candidates[j].IsSecurityFix {
			return candidates[i].IsSecurityFix
		}
		return candidates[i].Package < candidates[j].Package
	})
}

// queryOSV performs the real POST https://api.osv.dev/v1/query call for a
// single pinned dependency.
func queryOSV(ctx context.Context, client *http.Client, dep PinnedDependency) ([]osvVulnerability, error) {
	reqBody := osvQueryRequest{
		Version: dep.Version,
		Package: osvPackage{
			Name:      dep.Package,
			Ecosystem: pypiEcosystem,
		},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling OSV request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, osvQueryURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("building OSV request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("performing OSV request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("OSV returned status %d: %s", resp.StatusCode, string(body))
	}

	var parsed osvQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decoding OSV response: %w", err)
	}
	return parsed.Vulns, nil
}

// latestPyPIVersion performs the real GET https://pypi.org/pypi/<pkg>/json
// call and returns the version PyPI reports as current.
func latestPyPIVersion(ctx context.Context, client *http.Client, pkg string) (string, error) {
	url := fmt.Sprintf(pypiJSONURLFormat, pkg)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("building PyPI request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("performing PyPI request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("PyPI returned status %d: %s", resp.StatusCode, string(body))
	}

	var parsed pypiProjectResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("decoding PyPI response: %w", err)
	}
	return parsed.Info.Version, nil
}
