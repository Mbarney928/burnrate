package config

import (
	"log/slog"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "127.0.0.1:8080" || cfg.DBPath != "burnrate.db" {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	if cfg.LogLevel != slog.LevelInfo || cfg.RateLimitRPS != 20 || cfg.RateLimitBurst != 40 || cfg.AuthFailuresPerMinute != 10 {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"BURNRATE_ADDR":                     "0.0.0.0:9000",
		"BURNRATE_DB_PATH":                  "/data/x.db",
		"BURNRATE_LOG_LEVEL":                "debug",
		"BURNRATE_RATE_LIMIT_RPS":           "5.5",
		"BURNRATE_RATE_LIMIT_BURST":         "7",
		"BURNRATE_AUTH_FAILURES_PER_MINUTE": "3",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "0.0.0.0:9000" || cfg.DBPath != "/data/x.db" || cfg.LogLevel != slog.LevelDebug ||
		cfg.RateLimitRPS != 5.5 || cfg.RateLimitBurst != 7 || cfg.AuthFailuresPerMinute != 3 {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

func TestLoadReportsEveryProblem(t *testing.T) {
	_, err := Load(env(map[string]string{
		"BURNRATE_ADDR":                     "no-port",
		"BURNRATE_LOG_LEVEL":                "loud",
		"BURNRATE_RATE_LIMIT_RPS":           "-1",
		"BURNRATE_RATE_LIMIT_BURST":         "zero",
		"BURNRATE_AUTH_FAILURES_PER_MINUTE": "0",
	}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"BURNRATE_ADDR", "BURNRATE_LOG_LEVEL", "BURNRATE_RATE_LIMIT_RPS", "BURNRATE_RATE_LIMIT_BURST", "BURNRATE_AUTH_FAILURES_PER_MINUTE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}
