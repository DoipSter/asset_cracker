package main

import (
	"path/filepath"
	"testing"
)

// The parity gate for the second engine version, as a test. See tools/parity/README.md.
func TestParityWithPythonV2(t *testing.T) {
	fixtures, _ := filepath.Glob("../../../tools/parity/fixtures/*_v2_*")
	if len(fixtures) == 0 {
		t.Fatal("no v2 recordings to replay")
	}
	for _, folder := range fixtures {
		t.Run(filepath.Base(folder), func(t *testing.T) {
			if err := run(folder); err != nil {
				t.Fatal(err)
			}
		})
	}
}
