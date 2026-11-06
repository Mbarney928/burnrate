package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type cli struct {
	env map[string]string
}

func newCLI(t *testing.T) *cli {
	t.Helper()
	return &cli{env: map[string]string{"BURNRATE_DB_PATH": filepath.Join(t.TempDir(), "cli.db"), "BURNRATE_LOG_LEVEL": "error"}}
}

func (c *cli) run(ctx context.Context, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(ctx, args, &out, &errOut, func(k string) string { return c.env[k] })
	return code, out.String(), errOut.String()
}

func TestHelpAndUnknownCommand(t *testing.T) {
	c := newCLI(t)
	for _, args := range [][]string{nil, {"help"}, {"--help"}} {
		if code, out, _ := c.run(context.Background(), args...); code != 0 || !strings.Contains(out, "Commands:") {
			t.Errorf("help %v: code %d", args, code)
		}
	}
	if code, _, errOut := c.run(context.Background(), "explode"); code != 2 || !strings.Contains(errOut, "unknown command") {
		t.Errorf("unknown command: code %d, %q", code, errOut)
	}
}

func TestInvalidConfigFailsFast(t *testing.T) {
	c := newCLI(t)
	c.env["BURNRATE_ADDR"] = "nope"
	if code, _, errOut := c.run(context.Background(), "migrate"); code != 2 || !strings.Contains(errOut, "BURNRATE_ADDR") {
		t.Errorf("code %d, %q", code, errOut)
	}
}

func TestDatabaseErrorsExitOne(t *testing.T) {
	c := newCLI(t)
	c.env["BURNRATE_DB_PATH"] = filepath.Join(t.TempDir(), "missing", "x.db")
	for _, args := range [][]string{{"migrate"}, {"seed"}, {"report"}, {"serve"}, {"token", "create", "--name", "x"}} {
		if code, _, _ := c.run(context.Background(), args...); code != 1 {
			t.Errorf("%v with unopenable DB: code %d, want 1", args, code)
		}
	}
}

var tokenRE = regexp.MustCompile(`brt_[A-Za-z0-9_-]{43}`)

func TestSeedReportAndTokens(t *testing.T) {
	ctx := context.Background()
	c := newCLI(t)

	if code, out, _ := c.run(ctx, "migrate"); code != 0 || !strings.Contains(out, "up to date") {
		t.Fatalf("migrate: %d %q", code, out)
	}
	if code, out, _ := c.run(ctx, "report"); code != 0 || !strings.Contains(out, "No SLOs yet") {
		t.Fatalf("empty report: %d %q", code, out)
	}

	code, out, errOut := c.run(ctx, "seed")
	if code != 0 {
		t.Fatalf("seed: %d %s", code, errOut)
	}
	if got := tokenRE.FindAllString(out, -1); len(got) != 2 || got[0] == got[1] {
		t.Fatalf("seed should print two distinct tokens, got %d", len(got))
	}
	if code, _, errOut := c.run(ctx, "seed"); code != 1 || !strings.Contains(errOut, "already contains") {
		t.Errorf("reseed: %d %q", code, errOut)
	}

	code, out, _ = c.run(ctx, "report")
	if code != 0 {
		t.Fatalf("report exit %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[0], "SERVICE") {
		t.Fatalf("report:\n%s", out)
	}
	for want, line := range map[string]string{"search-api": lines[1], "notifications-worker": lines[2]} {
		if !strings.HasPrefix(line, want) {
			t.Errorf("expected %s in urgency order, got %q", want, line)
		}
	}
	if !strings.Contains(lines[1], "PAGE") || !strings.Contains(lines[2], "TICKET") || !strings.Contains(out, "exhausted") {
		t.Errorf("report severities wrong:\n%s", out)
	}

	before := time.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if code, out, _ := c.run(ctx, "report", "--at", before); code != 0 || strings.Contains(out, "PAGE") {
		t.Errorf("historical report should have no pages: %d\n%s", code, out)
	}
	if code, _, _ := c.run(ctx, "report", "--at", "tuesday"); code != 2 {
		t.Errorf("bad --at: code %d", code)
	}
	if code, _, _ := c.run(ctx, "report", "--bogus"); code != 2 {
		t.Errorf("bad flag: code %d", code)
	}

	code, out, _ = c.run(ctx, "token", "create", "--name", "ci-bot", "--role", "editor")
	if code != 0 || len(tokenRE.FindAllString(out, -1)) != 1 {
		t.Fatalf("token create: %d %q", code, out)
	}
	for _, args := range [][]string{
		{"token"},
		{"token", "list"},
		{"token", "create"},
		{"token", "create", "--name", "x", "--role", "admin"},
		{"token", "create", "--name", "ci-bot"},
		{"token", "create", "--wat"},
	} {
		if code, _, _ := c.run(ctx, args...); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}
}

func TestServeAnswersHealthAndShutsDown(t *testing.T) {
	c := newCLI(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	c.env["BURNRATE_ADDR"] = addr

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		code, _, _ := c.run(ctx, "serve")
		done <- code
	}()

	url := fmt.Sprintf("http://%s/healthz", addr)
	var ok bool
	for range 50 {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if res, err := http.DefaultClient.Do(req); err == nil {
			ok = res.StatusCode == http.StatusOK
			_ = res.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if !ok {
		t.Fatal("server never answered /healthz")
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("serve exited %d after shutdown", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not shut down")
	}
}

func TestServeFailsWhenAddressInUse(t *testing.T) {
	c := newCLI(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c.env["BURNRATE_ADDR"] = ln.Addr().String()
	if code, _, _ := c.run(context.Background(), "serve"); code != 1 {
		t.Errorf("serve on a busy port: code %d, want 1", code)
	}
}

func TestFormatHours(t *testing.T) {
	if formatHours(5) != "5.0h" || formatHours(72) != "3.0d" {
		t.Errorf("formatHours: %q %q", formatHours(5), formatHours(72))
	}
}
