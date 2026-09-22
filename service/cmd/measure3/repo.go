package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The repository side of the protocol: which commit is T_c, whether the text has moved since,
// and the errata rules of section 0.1. Every check is a refusal to run, not a warning.

const (
	protocolPath = "docs/v3-measurement-protocol.md"
	errataPath   = "docs/v3-measurement-protocol-errata.md"
	researchDir  = "research/v3"

	// protocolSHA is the sha-256 of the protocol text this tool was written against: the third
	// amendment (drift_tol as the fact 0), 2026-09-22. The tool refuses to run against any other
	// text: a new amendment is a new protocol, and the tool is read again with it (section 6,
	// last line). The second amendment's text was 8449e67337ee...; the tool's first run against
	// the record (research/v3/runs.jsonl, 07:10 UTC) was under that text and froze nothing.
	protocolSHA = "46017747d9a45c833a7b17da25b6b3e199657f7ee1d7d1a635d611fd551c2dbf"

	noErrataLine = "No errata yet."
)

// repo is the checkout the tool runs from.
type repo struct {
	Root string
}

// findRepo walks up from dir to the checkout that holds the protocol file.
func findRepo(dir string) (repo, error) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return repo{}, err
	}
	for {
		if _, err := os.Stat(filepath.Join(d, protocolPath)); err == nil {
			return repo{Root: d}, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return repo{}, fmt.Errorf("no checkout holding %s above %s", protocolPath, dir)
		}
		d = parent
	}
}

func (r repo) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Root
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func sha256File(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// protocolCommit is T_c: the committer time and hash of the last commit that changed the
// protocol file (section 1). It refuses a file in no commit, a file changed since, and a file
// whose text is not the one this tool embeds.
func (r repo) protocolCommit() (hash string, tc time.Time, err error) {
	out, err := r.git("log", "-1", "--format=%H %cI", "--", protocolPath)
	if err != nil {
		return "", tc, err
	}
	if out == "" {
		return "", tc, fmt.Errorf("refusing: %s is in no commit", protocolPath)
	}
	parts := strings.Fields(out)
	if len(parts) != 2 {
		return "", tc, fmt.Errorf("unexpected git log output %q", out)
	}
	tc, err = time.Parse(time.RFC3339, parts[1])
	if err != nil {
		return "", tc, fmt.Errorf("T_c: %w", err)
	}
	if dirty, err := r.dirty(protocolPath); err != nil {
		return "", tc, err
	} else if dirty {
		return "", tc, fmt.Errorf("refusing: %s has changed since its commit", protocolPath)
	}
	sum, err := sha256File(filepath.Join(r.Root, protocolPath))
	if err != nil {
		return "", tc, err
	}
	if sum != protocolSHA {
		return "", tc, fmt.Errorf("refusing: %s is not the text this tool was written against (sha %s, tool %s): a new amendment needs the tool read again", protocolPath, sum[:12], protocolSHA[:12])
	}
	return parts[0], tc.UTC(), nil
}

func (r repo) dirty(path string) (bool, error) {
	out, err := r.git("status", "--porcelain", "--", path)
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// errataState is what the tool records about the errata file (0.1).
type errataState struct {
	SHA     string   `json:"sha256"`
	Commit  string   `json:"commit"`
	Entries []string `json:"entries"` // the entry headings in force
}

// checkErrata applies 0.1's rules: the file exists and is clean, no commit to it ever deleted a
// line other than "No errata yet.", and its last commit is held by the remote.
func (r repo) checkErrata() (errataState, error) {
	path := filepath.Join(r.Root, errataPath)
	if _, err := os.Stat(path); err != nil {
		return errataState{}, fmt.Errorf("refusing: %s: %w", errataPath, err)
	}
	if dirty, err := r.dirty(errataPath); err != nil {
		return errataState{}, err
	} else if dirty {
		return errataState{}, fmt.Errorf("refusing: %s has uncommitted changes", errataPath)
	}
	patch, err := r.git("log", "-p", "--format=", "--", errataPath)
	if err != nil {
		return errataState{}, err
	}
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "---") || !strings.HasPrefix(line, "-") {
			continue
		}
		if strings.TrimSpace(line[1:]) != noErrataLine {
			return errataState{}, fmt.Errorf("refusing: a commit to %s deleted the line %q; errata are append-only", errataPath, line[1:])
		}
	}
	last, err := r.git("log", "-1", "--format=%H", "--", errataPath)
	if err != nil {
		return errataState{}, err
	}
	if last == "" {
		return errataState{}, fmt.Errorf("refusing: %s is in no commit", errataPath)
	}
	held, err := r.git("branch", "-r", "--contains", last)
	if err != nil {
		return errataState{}, err
	}
	if held == "" {
		return errataState{}, fmt.Errorf("refusing: the remote does not hold commit %s of %s; push first", last[:12], errataPath)
	}
	sum, err := sha256File(path)
	if err != nil {
		return errataState{}, err
	}
	entries, err := errataEntries(path)
	if err != nil {
		return errataState{}, err
	}
	return errataState{SHA: sum, Commit: last, Entries: entries}, nil
}

// errataEntries lists the entry headings (lines starting "## ") after the preamble; none while
// the file still says "No errata yet.".
func errataEntries(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "## ") {
			out = append(out, strings.TrimSpace(line[3:]))
		}
	}
	if out == nil {
		out = []string{}
	}
	return out, sc.Err()
}

func (r repo) headSHA() (string, error) { return r.git("rev-parse", "HEAD") }
