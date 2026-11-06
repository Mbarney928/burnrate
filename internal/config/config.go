// Package config loads and validates runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
)

// Config is the validated runtime configuration for burnrate.
type Config struct {
	// Addr is the host:port the HTTP server listens on.
	Addr string
	// DBPath is the SQLite database file.
	DBPath string
	// LogLevel controls the minimum level of structured logs.
	LogLevel slog.Level
	// RateLimitRPS is the sustained request rate allowed per client IP.
	RateLimitRPS float64
	// RateLimitBurst is the short burst allowed per client IP.
	RateLimitBurst int
	// AuthFailuresPerMinute caps failed authentication attempts per client IP.
	AuthFailuresPerMinute int
}

// Load reads configuration through getenv (os.Getenv in production) and validates it.
// Every problem is reported at once so a misconfigured deployment fails fast with a clear message.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		Addr:                  orDefault(getenv("BURNRATE_ADDR"), "127.0.0.1:8080"),
		DBPath:                orDefault(getenv("BURNRATE_DB_PATH"), "burnrate.db"),
		RateLimitRPS:          20,
		RateLimitBurst:        40,
		AuthFailuresPerMinute: 10,
	}
	var errs []error

	if _, _, err := net.SplitHostPort(cfg.Addr); err != nil {
		errs = append(errs, fmt.Errorf("BURNRATE_ADDR must be host:port: %w", err))
	}
	if strings.TrimSpace(cfg.DBPath) == "" {
		errs = append(errs, errors.New("BURNRATE_DB_PATH must not be blank"))
	}

	if v := getenv("BURNRATE_LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("BURNRATE_LOG_LEVEL must be debug, info, warn or error, got %q", v))
		}
	}
	if v := getenv("BURNRATE_RATE_LIMIT_RPS"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || f > 10000 {
			errs = append(errs, fmt.Errorf("BURNRATE_RATE_LIMIT_RPS must be a number in (0, 10000], got %q", v))
		}
		cfg.RateLimitRPS = f
	}
	if v := getenv("BURNRATE_RATE_LIMIT_BURST"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100000 {
			errs = append(errs, fmt.Errorf("BURNRATE_RATE_LIMIT_BURST must be an integer in [1, 100000], got %q", v))
		}
		cfg.RateLimitBurst = n
	}
	if v := getenv("BURNRATE_AUTH_FAILURES_PER_MINUTE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			errs = append(errs, fmt.Errorf("BURNRATE_AUTH_FAILURES_PER_MINUTE must be an integer in [1, 1000], got %q", v))
		}
		cfg.AuthFailuresPerMinute = n
	}

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
