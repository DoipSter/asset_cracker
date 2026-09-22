package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// The files under research/v3 (sections 4.1, 6 and 8). Every write goes to a temporary name and
// is renamed into place after fsync, so a crash leaves either the old file or the new one.

const (
	frozenFile        = "frozen-params.json"
	trainResultFile   = "train-result.json"
	trainProgressFile = "train-progress.json" // an incomplete TRAIN's figures: they freeze nothing
	testAttemptFile   = "test-attempt.json"
	testResultFile    = "test-result.json"
	runsFile          = "runs.jsonl"
)

// provenanceRecord ties a result to the texts it was measured under.
type provenanceRecord struct {
	ToolGitSHA   string      `json:"tool_git_sha"`
	ProtocolSHA  string      `json:"protocol_sha256"`
	Errata       errataState `json:"errata"`
	TC           time.Time   `json:"t_c"`
	TCCommit     string      `json:"t_c_commit"`
	T0           time.Time   `json:"t0"`
	RunAt        time.Time   `json:"run_at"`
	FirstProdRun string      `json:"first_prod_run_tool_sha,omitempty"`
}

// frozenParams is 4.1: what train freezes, and nothing else (R3).
type frozenParams struct {
	Lambda        float64 `json:"lambda"`
	StaleCost     float64 `json:"stale_cost"`
	StaleCostSell float64 `json:"stale_cost_sell"`
	BlockSeconds  int64   `json:"block_seconds"`
	EmbargoWin    int     `json:"embargo_windows"`

	TrainFirstClose time.Time         `json:"train_first_close"`
	TrainLastClose  time.Time         `json:"train_last_close"`
	EmbargoEnd      time.Time         `json:"embargo_end"`
	TrainWindows    []int64           `json:"train_windows"`
	Ineligible      map[string]string `json:"ineligible_windows"` // key -> reason

	TC          time.Time `json:"t_c"`
	TCCommit    string    `json:"t_c_commit"`
	ProtocolSHA string    `json:"protocol_sha256"`
	ErrataSHA   string    `json:"errata_sha256"`
	FrozenAt    time.Time `json:"frozen_at"`
	ToolGitSHA  string    `json:"tool_git_sha"`
}

// trainResult is section 8's list for train.
type trainResult struct {
	Provenance   provenanceRecord     `json:"provenance"`
	Trials       int                  `json:"trials"`
	Coverage     map[string]time.Time `json:"coverage"` // s_c per coin
	Complete     bool                 `json:"train_complete"`
	WindowsFound int                  `json:"eligible_windows_found"`
	Split        struct {
		FirstClose   time.Time         `json:"first_close"`
		LastClose    time.Time         `json:"last_close"`
		EmbargoEnd   time.Time         `json:"embargo_end"`
		Windows      []int64           `json:"windows"`
		Ineligible   map[string]string `json:"ineligible"`
		LateEligible []int64           `json:"became_eligible_after_the_freeze"`
	} `json:"split"`
	M1              m1Report       `json:"m1"`
	Autocorrelation autocorrReport `json:"autocorrelation"`
	M2              staleReport    `json:"m2_stale_cost"`
	M2s             staleReport    `json:"m2s_stale_cost_sell"`
	M3              m3Report       `json:"m3_outcome_correlation"`
	Power           powerReport    `json:"r2_power"`
	Frozen          *frozenParams  `json:"frozen,omitempty"`
	Reproduced      *bool          `json:"reproduced_frozen_digits,omitempty"`
	Notes           []string       `json:"notes"`
}

// testAttempt is written BEFORE the look (section 6).
type testAttempt struct {
	Attempts []struct {
		At     time.Time `json:"at"`
		Reason string    `json:"reason,omitempty"`
	} `json:"attempts"`
	ToolGitSHA  string            `json:"tool_git_sha"`
	ProtocolSHA string            `json:"protocol_sha256"`
	ErrataSHA   string            `json:"errata_sha256"`
	Windows     []int64           `json:"windows"`
	Ineligible  map[string]string `json:"ineligible"`
}

type testResult struct {
	Provenance provenanceRecord    `json:"provenance"`
	Attempt    testAttempt         `json:"attempt"`
	Frozen     frozenParams        `json:"frozen"`
	R2         r2Report            `json:"r2"`
	ByCoin     map[string]r2Report `json:"by_coin"`
	Notes      []string            `json:"notes"`
}

// runRecord is one line of runs.jsonl: every run, complete or not.
type runRecord struct {
	At             time.Time `json:"at"`
	Command        string    `json:"command"`
	ToolGitSHA     string    `json:"tool_git_sha"`
	Complete       bool      `json:"complete"`
	FiguresPrinted bool      `json:"figures_printed"`
	Note           string    `json:"note"`
}

func (r repo) researchPath(name string) string { return filepath.Join(r.Root, researchDir, name) }

// writeJSON writes v with every NaN or infinite float as null: a statistic that could not be
// computed (one block, no pair, no variance) is recorded as absent, not as a number.
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(nullNaN(v), "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func appendRun(path string, rec runRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "%s\n", b); err != nil {
		return err
	}
	return f.Sync()
}

func keyString(w int64) string { return time.Unix(w, 0).UTC().Format(time.RFC3339) }

// nullNaN walks v by reflection and returns a copy in which every float64 that is NaN or
// infinite is a nil pointer, which encoding/json writes as null. Structs become maps keyed by
// their json names, so the shape of the file is unchanged.
func nullNaN(v any) any {
	return nullNaNValue(reflect.ValueOf(v))
}

func nullNaNValue(rv reflect.Value) any {
	switch rv.Kind() {
	case reflect.Invalid:
		return nil
	case reflect.Float64, reflect.Float32:
		f := rv.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil
		}
		return f
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return nil
		}
		return nullNaNValue(rv.Elem())
	case reflect.Struct:
		if t, ok := rv.Interface().(time.Time); ok {
			return t
		}
		out := map[string]any{}
		rt := rv.Type()
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			name, omit := f.Name, false
			if tag := f.Tag.Get("json"); tag != "" {
				parts := strings.Split(tag, ",")
				if parts[0] == "-" {
					continue
				}
				if parts[0] != "" {
					name = parts[0]
				}
				for _, p := range parts[1:] {
					if p == "omitempty" {
						omit = true
					}
				}
			}
			fv := rv.Field(i)
			if omit && fv.IsZero() {
				continue
			}
			out[name] = nullNaNValue(fv)
		}
		return out
	case reflect.Map:
		out := map[string]any{}
		iter := rv.MapRange()
		for iter.Next() {
			out[fmt.Sprint(iter.Key().Interface())] = nullNaNValue(iter.Value())
		}
		return out
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return []any{}
		}
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = nullNaNValue(rv.Index(i))
		}
		return out
	default:
		return rv.Interface()
	}
}
