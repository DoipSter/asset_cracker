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

// The repository side of docs/calibration-protocol.md: which commit is T_c, whether the text has
// moved since, the errata rules (section 0.1 of the v3 protocol, applied to this one), and the
// commits the protocol asks the tool to make. Every check is a refusal to run, not a warning.
// The functions follow cmd/measure3's, which the v3 protocol's 0.1 covers and which is not
// touched here.

const (
	protocolPath = "docs/calibration-protocol.md"
	errataPath   = "docs/calibration-protocol-errata.md"
	researchDir  = "research/calibration"

	// protocolSHA is the sha-256 of the text this tool was written against: the amendment of
	// 2026-09-25 (commit 3d7fd96, "Conventions fixed before TRAIN"). Any other text is a new
	// protocol, and the tool is read again with it.
	protocolSHA = "1d3fdf3a36762632a90df71d64b9971d470836556797c23f94c600ad86e52fc7"

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
// protocol file. It refuses a file in no commit, a file changed since, and a text other than the
// one this tool embeds.
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
		return "", tc, fmt.Errorf("refusing: %s is not the text this tool was written against (sha %s, tool %s): a new protocol needs the tool read again", protocolPath, sum[:12], protocolSHA[:12])
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

// errataState is what the tool records about the errata file.
type errataState struct {
	SHA     string   `json:"sha256"`
	Commit  string   `json:"commit"`
	Entries []string `json:"entries"` // the entry headings in force
}

// checkErrata: the file exists and is clean, no commit to it ever deleted a line other than
// "No errata yet.", and its last commit is held by the remote.
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
	if held, err := r.heldByRemote(last); err != nil {
		return errataState{}, err
	} else if !held {
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

// errataEntries lists the entry headings (lines starting "## "); none while the file still says
// "No errata yet.".
func errataEntries(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := []string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "## ") {
			out = append(out, strings.TrimSpace(line[3:]))
		}
	}
	return out, sc.Err()
}

func (r repo) headSHA() (string, error) { return r.git("rev-parse", "HEAD") }

// heldByRemote says whether any remote-tracking branch contains the commit.
func (r repo) heldByRemote(commit string) (bool, error) {
	out, err := r.git("branch", "-r", "--contains", commit)
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// committedFile is the last commit that changed a research file, "" if none, refusing a file
// that differs from that commit.
func (r repo) committedFile(rel string) (string, error) {
	last, err := r.git("log", "-1", "--format=%H", "--", rel)
	if err != nil || last == "" {
		return "", err
	}
	if dirty, err := r.dirty(rel); err != nil {
		return "", err
	} else if dirty {
		return "", fmt.Errorf("refusing: %s differs from its last commit", rel)
	}
	return last, nil
}

// commit records the tool's own files and nothing else, whatever else is staged: the protocol
// asks the tool to commit what it writes (the attempt before any outcome is read, then the
// result). It returns the commit's hash.
func (r repo) commit(message string, rels ...string) (string, error) {
	args := append([]string{"add", "--"}, rels...)
	if _, err := r.git(args...); err != nil {
		return "", err
	}
	args = append([]string{"commit", "-q", "-m", message, "--"}, rels...)
	if _, err := r.git(args...); err != nil {
		return "", err
	}
	return r.headSHA()
}
