// Package config reads the service's settings from the environment.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Config is everything the service needs to be told from outside.
type Config struct {
	// DatabaseURL is a libpq connection string. On the Pi the service reaches Postgres over
	// the local unix socket and is authenticated as its operating-system user, so there is
	// no password to configure.
	DatabaseURL string
	// HTTPAddr is where the health endpoint listens. Localhost only by default: the service
	// is never exposed to the network directly.
	HTTPAddr string
	// UserAgent is sent with every request to a venue.
	UserAgent string
	// V3 is the switch for the live engine's NEW ORDERS, from AC_V3. The default is on (unset or
	// empty). Only the exact value "off" disables new orders. A bet already held is still valued
	// and settled either way (docs/honest-fills-v3.md, 5.4).
	V3 bool
	// V3Faults is database-fault injection for the third engine, from AC_V3_FAIL_EVERY,
	// AC_V3_FAIL_RUN, AC_V3_FAIL_KIND, AC_V3_FAIL_OPS and AC_V3_DELAY_COMMIT. It is read in every
	// build and HONOURED ONLY by a binary built with the tag "faultinject" (runner.WrapStore3);
	// a normal build logs that it was asked and ignores it. Dev only: the dev and production
	// databases share one Postgres cluster, so stopping the server is not a test anyone may run.
	V3Faults V3Faults
	// Gate is what the operator sets of the promotion gate (analysis.GateSettings), from
	// AC_GATE_MIN_EDGE (after-fee return per dollar staked worth finding), AC_GATE_POWER and
	// AC_GATE_MAX_DRAWDOWN_CENTS. A variable that is unset or does not parse is 0 here, and
	// cmd/assetcracker keeps the analysis package's default for that one setting. The defaults
	// live there, not here, so that they are stated once.
	Gate Gate
	// CatalogueCategories are the Kalshi categories the assets page's catalogue lists, from
	// AC_CATALOGUE_CATEGORIES (comma-separated, as Kalshi spells them). MaxSelected is the most
	// instruments the assets page may have switched on at once, from AC_MAX_SELECTED. Unset, empty
	// or not a positive number is nil or 0 here, and the catalogue package's default applies
	// (Crypto only; 10), stated there once.
	CatalogueCategories []string
	MaxSelected         int
}

// Gate mirrors analysis.GateSettings. It is a type of this package so that config imports
// nothing of ours.
type Gate struct {
	MinEdgePerDollar float64
	Power            float64
	MaxDrawdownCents int64
}

// V3Faults mirrors runner.Faults, which says what each field does. It is a type of this package
// so that config imports nothing of ours.
type V3Faults struct {
	Every       int
	Run         int
	Kind        string // refuse | unknown | lose | delay
	DelayCommit time.Duration
	Ops         []string // orders | settlements | reads
}

// DatabaseName is the database the service connects to, worked out from DatabaseURL by the same
// parser pgx connects with (so PGDATABASE and a libpq keyword string are honoured as pgx honours
// them). "" if the URL cannot be parsed.
//
// It is an INFERENCE from the connection string, not a reading of current_database(): the store
// package, which is frozen for this step, has no call that asks the server. The third engine
// uses it for one thing only, to refuse dev plumbing versions anywhere but a database whose name
// ends in "_dev"; tools/dev-v3-plumbing.sql, which creates those versions, does ask the server.
func (c Config) DatabaseName() string {
	cfg, err := pgconn.ParseConfig(c.DatabaseURL)
	if err != nil {
		return ""
	}
	return cfg.Database
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Load returns the configuration, with defaults that suit the Pi.
func Load() Config {
	c := Config{
		DatabaseURL: env("AC_DATABASE_URL", "postgres:///assetcracker?host=/var/run/postgresql"),
		HTTPAddr:    env("AC_HTTP_ADDR", "127.0.0.1:8377"),
		UserAgent:   env("AC_USER_AGENT", "asset-cracker/0.1"),
		V3:          os.Getenv("AC_V3") != "off",
	}
	c.V3Faults.Every, _ = strconv.Atoi(os.Getenv("AC_V3_FAIL_EVERY"))
	c.V3Faults.Run, _ = strconv.Atoi(os.Getenv("AC_V3_FAIL_RUN"))
	c.V3Faults.Kind = os.Getenv("AC_V3_FAIL_KIND")
	if d, err := time.ParseDuration(os.Getenv("AC_V3_DELAY_COMMIT")); err == nil && d > 0 {
		c.V3Faults.DelayCommit = d
		if c.V3Faults.Kind == "" {
			c.V3Faults.Kind = "delay"
		}
	}
	for _, op := range strings.Split(os.Getenv("AC_V3_FAIL_OPS"), ",") {
		if op = strings.TrimSpace(op); op != "" {
			c.V3Faults.Ops = append(c.V3Faults.Ops, op)
		}
	}
	c.Gate.MinEdgePerDollar, _ = strconv.ParseFloat(os.Getenv("AC_GATE_MIN_EDGE"), 64)
	c.Gate.Power, _ = strconv.ParseFloat(os.Getenv("AC_GATE_POWER"), 64)
	c.Gate.MaxDrawdownCents, _ = strconv.ParseInt(os.Getenv("AC_GATE_MAX_DRAWDOWN_CENTS"), 10, 64)
	for _, cat := range strings.Split(os.Getenv("AC_CATALOGUE_CATEGORIES"), ",") {
		if cat = strings.TrimSpace(cat); cat != "" {
			c.CatalogueCategories = append(c.CatalogueCategories, cat)
		}
	}
	if n, err := strconv.Atoi(os.Getenv("AC_MAX_SELECTED")); err == nil && n > 0 {
		c.MaxSelected = n
	}
	return c
}
