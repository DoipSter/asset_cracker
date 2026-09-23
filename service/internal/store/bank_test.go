package store

import (
	"testing"
	"time"
)

func TestAdvancePaydaySkipsTheBacklog(t *testing.T) {
	due := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := due.AddDate(0, 0, 100)
	next := advancePayday(due, now, 14)
	if !next.After(now) {
		t.Fatalf("next %s is not after %s", next, now)
	}
	// 14-day steps from the due date: 7 steps land on day 98, which is still before day 100,
	// so the schedule continues at day 112. One payment, not seven.
	want := due.AddDate(0, 0, 112)
	if !next.Equal(want) {
		t.Fatalf("next %s, want %s", next, want)
	}
}

func TestAdvancePaydayOneStepWhenItIsDue(t *testing.T) {
	due := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	now := due.Add(time.Hour)
	next := advancePayday(due, now, 14)
	want := due.AddDate(0, 0, 14)
	if !next.Equal(want) {
		t.Fatalf("next %s, want %s", next, want)
	}
}
