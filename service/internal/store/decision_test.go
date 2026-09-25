package store

import "testing"

// A probability the engine did not form is stored as NULL; one it formed is stored as it is,
// an exact 0 included (a model far from the strike can say 0).
func TestProbOrNull(t *testing.T) {
	if got := probOrNull(true, 0.6); got != nil {
		t.Errorf("missing: %v", got)
	}
	if got, ok := probOrNull(false, 0).(float64); !ok || got != 0 {
		t.Errorf("a formed 0: %v", probOrNull(false, 0))
	}
	if got, ok := probOrNull(false, 0.42).(float64); !ok || got != 0.42 {
		t.Errorf("formed: %v", probOrNull(false, 0.42))
	}
}
