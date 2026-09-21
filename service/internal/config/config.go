// Package config reads the service's settings from the environment.
package config

import (
	"os"
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
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Load returns the configuration, with defaults that suit the Pi.
func Load() Config {
	return Config{
		DatabaseURL: env("AC_DATABASE_URL", "postgres:///assetcracker?host=/var/run/postgresql"),
		HTTPAddr:    env("AC_HTTP_ADDR", "127.0.0.1:8377"),
		UserAgent:   env("AC_USER_AGENT", "asset-cracker/0.1"),
	}
}
