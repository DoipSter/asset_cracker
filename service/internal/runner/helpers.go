package runner

import (
	"strconv"
	"time"
)

func unix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func f(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
