package kalshi15m

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// Writes the SQL values for db/migrations/0004 from the Go parameters, so they cannot drift.
// Run with: AC_GEN=/path/to/out go test -run TestGenerateStrategySeed ./internal/kalshi15m/
func TestGenerateStrategySeed(t *testing.T) {
	out := os.Getenv("AC_GEN")
	if out == "" {
		t.Skip("set AC_GEN=<file> to write the seed rows")
	}
	var rows []string
	for _, p := range Strategies {
		b, _ := json.Marshal(p)
		rows = append(rows, fmt.Sprintf("    ('%s', '%s', '%s'::jsonb)", p.Name, p.Blurb, strings.ReplaceAll(string(b), "'", "''")))
	}
	if err := os.WriteFile(out, []byte(strings.Join(rows, ",\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
