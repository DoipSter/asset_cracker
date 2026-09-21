package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The parity gate as a test: the Go port of the six strategies must reproduce what the Python
// did with the same recorded inputs. See tools/parity/README.md.
func TestParityWithPython(t *testing.T) {
	fixtures, _ := filepath.Glob("../../../tools/parity/fixtures/*")
	if len(fixtures) == 0 {
		t.Fatal("no recordings to replay")
	}
	for _, folder := range fixtures {
		if st, err := os.Stat(folder); err != nil || !st.IsDir() {
			continue
		}
		t.Run(filepath.Base(folder), func(t *testing.T) {
			if err := run(folder); err != nil {
				t.Fatal(err)
			}
		})
	}
}
