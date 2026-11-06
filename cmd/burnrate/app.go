package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Mbarney928/burnrate/api"
	"github.com/Mbarney928/burnrate/internal/auth"
	"github.com/Mbarney928/burnrate/internal/config"
	"github.com/Mbarney928/burnrate/internal/httpapi"
	"github.com/Mbarney928/burnrate/internal/logging"
	"github.com/Mbarney928/burnrate/internal/model"
	"github.com/Mbarney928/burnrate/internal/ratelimit"
	"github.com/Mbarney928/burnrate/internal/seed"
	"github.com/Mbarney928/burnrate/internal/service"
	"github.com/Mbarney928/burnrate/internal/slo"
	"github.com/Mbarney928/burnrate/internal/store"
)

const usage = `burnrate tracks SLO error budgets and burn-rate alerts.

Usage:
  burnrate <command> [flags]

Commands:
  serve                              Run the HTTP API (applies migrations first)
  migrate                            Apply database migrations
  seed                               Load fictional demo data and print demo API tokens
  token create --name N --role R     Create an API token (role: viewer or editor)
  report [--at RFC3339]              Print the error-budget status of every SLO

Configuration comes from environment variables; see .env.example.
`

// app is everything a command needs, wired from configuration.
type app struct {
	cfg    config.Config
	log    *slog.Logger
	stdout io.Writer
	now    func() time.Time
}

// run executes the CLI and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	a := &app{cfg: cfg, log: logging.New(stderr, cfg.LogLevel), stdout: stdout, now: time.Now}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "serve":
		err = a.serve(ctx)
	case "migrate":
		err = a.migrate(ctx)
	case "seed":
		err = a.seed(ctx)
	case "token":
		err = a.token(ctx, rest)
	case "report":
		err = a.report(ctx, rest)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		var uerr usageError
		if errors.As(err, &uerr) {
			return 2
		}
		return 1
	}
	return 0
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// open connects to the database and applies pending migrations.
func (a *app) open(ctx context.Context) (*store.Store, error) {
	st, err := store.Open(ctx, a.cfg.DBPath)
	if err != nil {
		return nil, err
	}
	n, err := st.Migrate(ctx)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	if n > 0 {
		a.log.Info("applied migrations", "count", n)
	}
	return st, nil
}

func (a *app) migrate(ctx context.Context) error {
	st, err := a.open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	fmt.Fprintln(a.stdout, "database is up to date")
	return nil
}

func (a *app) serve(ctx context.Context) error {
	st, err := a.open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	handler := httpapi.NewRouter(httpapi.Deps{
		Service:      service.New(st, a.now),
		Tokens:       st,
		Health:       st.Ping,
		OpenAPI:      api.OpenAPI,
		Logger:       a.log,
		Limiter:      ratelimit.New(a.cfg.RateLimitRPS, a.cfg.RateLimitBurst, nil),
		AuthFailures: ratelimit.New(float64(a.cfg.AuthFailuresPerMinute)/60, a.cfg.AuthFailuresPerMinute, nil),
	})
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", a.cfg.Addr)
	if err != nil {
		return err
	}
	a.log.Info("listening", "addr", ln.Addr().String())

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	a.log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (a *app) seed(ctx context.Context) error {
	st, err := a.open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	now := a.now()
	toks, err := seed.Run(ctx, st, service.New(st, a.now), now)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Seeded %d fictional services with 30 days of synthetic traffic ending %s:\n",
		len(seed.Scenarios), now.UTC().Format(time.RFC3339))
	for _, sc := range seed.Scenarios {
		fmt.Fprintf(a.stdout, "  %-22s %s\n", sc.Service, sc.Story)
	}
	fmt.Fprintf(a.stdout, "\nDemo API tokens (shown once, stored only as hashes):\n  editor: %s\n  viewer: %s\n", toks.Editor, toks.Viewer)
	return nil
}

func (a *app) token(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return usageError{"usage: burnrate token create --name NAME --role viewer|editor"}
	}
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "token name")
	role := fs.String("role", string(model.RoleViewer), "viewer or editor")
	if err := fs.Parse(args[1:]); err != nil {
		return usageError{err.Error()}
	}
	*name = strings.TrimSpace(*name)
	if *name == "" || len(*name) > 64 {
		return usageError{"--name is required (at most 64 characters)"}
	}
	r := model.Role(*role)
	if !r.Valid() {
		return usageError{"--role must be viewer or editor"}
	}

	st, err := a.open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	plain, hash, err := auth.NewToken()
	if err != nil {
		return err
	}
	if _, err := st.CreateToken(ctx, *name, r, hash); err != nil {
		if errors.Is(err, model.ErrConflict) {
			return usageError{fmt.Sprintf("a token named %q already exists", *name)}
		}
		return err
	}
	fmt.Fprintf(a.stdout, "Created %s token %q. Copy it now; it cannot be shown again:\n%s\n", r, *name, plain)
	return nil
}

func (a *app) report(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	atFlag := fs.String("at", "", "evaluate as of this RFC 3339 instant (default now)")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	var at time.Time
	if *atFlag != "" {
		var err error
		if at, err = time.Parse(time.RFC3339, *atFlag); err != nil {
			return usageError{"--at must be an RFC 3339 timestamp"}
		}
	}

	st, err := a.open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	all, err := service.New(st, a.now).Overview(ctx, at)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Fprintln(a.stdout, "No SLOs yet. Run `burnrate seed` for demo data.")
		return nil
	}

	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tSLO\tTARGET\tSLI\tBUDGET LEFT\tBURN 1h\tBURN 6h\tBURN 3d\tSEVERITY\tEXHAUSTS IN")
	for _, s := range all {
		sli := "-"
		if s.Status.SLI != nil {
			sli = fmt.Sprintf("%.3f%%", *s.Status.SLI*100)
		}
		exhausts := "-"
		switch {
		case s.Status.Exhausted:
			exhausts = "exhausted"
		case s.Status.HoursToExhaustion != nil:
			exhausts = formatHours(*s.Status.HoursToExhaustion)
		}
		fmt.Fprintf(tw, "%s\t%s\t%v%%\t%s\t%.1f%%\t%.2f\t%.2f\t%.2f\t%s\t%s\n",
			s.SLO.ServiceName, s.SLO.Name, slo.Round(s.SLO.Target*100, 3), sli,
			s.Status.BudgetRemaining*100,
			burn(s.Status, "1h"), burn(s.Status, "6h"), burn(s.Status, "3d"),
			strings.ToUpper(string(s.Status.Severity)), exhausts)
	}
	return tw.Flush()
}

func burn(st slo.Status, window string) float64 {
	for _, b := range st.BurnRates {
		if b.Window == window {
			return b.BurnRate
		}
	}
	return 0
}

func formatHours(h float64) string {
	if h >= 48 {
		return fmt.Sprintf("%.1fd", h/24)
	}
	return fmt.Sprintf("%.1fh", h)
}
