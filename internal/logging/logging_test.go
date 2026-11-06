package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestNewWritesJSONAndRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelWarn)
	log.Info("hidden")
	log.Warn("shown", "k", "v")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("expected exactly one JSON record, got %q: %v", buf.String(), err)
	}
	if rec["msg"] != "shown" || rec["k"] != "v" || rec["level"] != "WARN" {
		t.Errorf("unexpected record: %v", rec)
	}
}
