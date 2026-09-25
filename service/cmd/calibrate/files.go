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

// The files under research/calibration ("The files"). Every write goes to a temporary name and is
// renamed into place after fsync, so a crash leaves the old file or the new one. The attempt is
// committed before any outcome is read; the result after.

const (
	trainAttemptFile = "train-attempt.json"
	trainResultFile  = "train-result.json"
	testAttemptFile  = "test-attempt.json"
	testResultFile   = "test-result.json"
	runsFile         = "runs.jsonl"
	longShotPath     = "docs/longshot-protocol.md"
)

// provenance ties a file to the texts and the commit it was made under.
type provenance struct {
	ToolGitSHA  string      `json:"tool_git_sha"`
	ProtocolSHA string      `json:"protocol_sha256"`
	TC          time.Time   `json:"t_c"`
	TCCommit    string      `json:"t_c_commit"`
	Errata      errataState `json:"errata"`
	Span        span        `json:"span"`
	RunAt       time.Time   `json:"run_at"`
}

type attemptRecord struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"` // "" for the first; a quoted mechanical failure for a rerun
}

// obsKey names an evaluation row the attempt will read.
type obsKey struct {
	EvaluationID int64 `json:"evaluation_id"`
	MarketID     int64 `json:"market_id"`
	Bin          int   `json:"bin"`
}

// attempt is written and committed BEFORE any outcome is read.
type attempt struct {
	Provenance   provenance      `json:"provenance"`
	Attempts     []attemptRecord `json:"attempts"`
	Rounds       []round         `json:"rounds"`
	LeftOut      []round         `json:"rounds_left_out_no_result"`
	Observations []obsKey        `json:"observations"`
	H15Markets   []h15Market     `json:"h15_markets,omitempty"` // test only
}

// coefficient is one term's fitted value.
type coefficient struct {
	Name           string  `json:"name"`
	Standardised   float64 `json:"standardised"`
	Unstandardised float64 `json:"unstandardised"` // the intercept's is not reported: its scale is the centred design's
	ClusteredSE    float64 `json:"clustered_se_standardised"`
}

type binInSample struct {
	Bin     string  `json:"bin"`
	Figures figures `json:"figures"`
}

// trainResult is what TRAIN freezes and reports.
type trainResult struct {
	Provenance    provenance     `json:"provenance"`
	AttemptCommit string         `json:"attempt_commit"`
	Observations  int            `json:"observations"`
	Rounds        int            `json:"rounds"`
	CloseTimes    int            `json:"close_times"`
	Model         model          `json:"model"`
	Coefficients  []coefficient  `json:"coefficients"`
	B             float64        `json:"b_logit_mid"`
	C             float64        `json:"c_logit_p_model"`
	Monotone      bool           `json:"monotone"`
	MonotoneNote  string         `json:"monotone_note"`
	InSample      figures        `json:"in_sample_overall"`
	InSampleBins  []binInSample  `json:"in_sample_by_bin"`
	Conventions   map[string]any `json:"conventions"`
}

// testResult is the one look at TEST.
type testResult struct {
	Provenance        provenance `json:"provenance"`
	TrainResultCommit string     `json:"train_result_commit"`
	AttemptCommit     string     `json:"attempt_commit"`
	Verdict           verdict    `json:"verdict"`
	Monotone          bool       `json:"train_monotone"`
	Registrable       bool       `json:"registrable"` // useful AND b, c >= 0: "How a yes becomes a strategy"
	Answer            string     `json:"answer"`      // "yes" or "no"
	H15               h15Report  `json:"h15"`
}

type runRecord struct {
	At      time.Time `json:"at"`
	Command string    `json:"command"`
	ToolSHA string    `json:"tool_git_sha"`
	Step    string    `json:"step"`
	Error   string    `json:"error,omitempty"`
}

func (r repo) researchRel(name string) string {
	return filepath.ToSlash(filepath.Join(researchDir, name))
}

func (r repo) researchPath(name string) string { return filepath.Join(r.Root, researchDir, name) }

// writeJSON writes v with every NaN or infinite float as null.
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

// nullNaN is a copy of v in which every NaN or infinite float is null (cmd/measure3's).
func nullNaN(v any) any { return nullNaNValue(reflect.ValueOf(v)) }

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
