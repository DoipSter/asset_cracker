package main

import (
	"fmt"
	"time"
)

// The split of docs/calibration-protocol.md ("Data and the split"): TRAIN is the first 14
// complete UTC days after T_c, TEST the next 14. A round belongs to the span its close falls in;
// a span is [From, To). A span runs no earlier than one hour after its last close ("When a span
// runs", [CONVENTION]).

const (
	spanDays = 14
	runAfter = time.Hour
)

type span struct {
	Name string    `json:"name"` // "train" or "test"
	From time.Time `json:"from"` // the first close counted
	To   time.Time `json:"to"`   // the first close not counted
}

// spans is TRAIN and TEST for a T_c.
func spans(tc time.Time) (train, test span) {
	day := tc.UTC().Truncate(24 * time.Hour)
	if !day.Equal(tc.UTC()) {
		day = day.Add(24 * time.Hour) // the first COMPLETE day after T_c
	}
	train = span{Name: "train", From: day, To: day.Add(spanDays * 24 * time.Hour)}
	test = span{Name: "test", From: train.To, To: train.To.Add(spanDays * 24 * time.Hour)}
	return train, test
}

// ready refuses a span whose rounds are not all past their closes by runAfter.
func (s span) ready(now time.Time) error {
	if at := s.To.Add(runAfter); now.Before(at) {
		return fmt.Errorf("refusing: %s holds the closes %s to %s and runs no earlier than %s (now %s)",
			s.Name, s.From.Format(time.RFC3339), s.To.Format(time.RFC3339), at.Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	return nil
}
