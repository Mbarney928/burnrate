// Package seed loads fictional demo data: four services of an invented online store,
// each with 30 days of synthetic traffic that tells a different error-budget story.
package seed

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"time"

	"github.com/Mbarney928/burnrate/internal/auth"
	"github.com/Mbarney928/burnrate/internal/model"
	"github.com/Mbarney928/burnrate/internal/service"
	"github.com/Mbarney928/burnrate/internal/slo"
	"github.com/Mbarney928/burnrate/internal/store"
)

// ErrAlreadySeeded is returned when the database already contains services.
var ErrAlreadySeeded = errors.New("database already contains services; seed only an empty database")

// Scenario is one fictional service with an SLO and a synthetic traffic pattern.
type Scenario struct {
	Service     string
	Team        string
	Description string
	SLO         string
	Target      float64
	WindowDays  int
	// PerMinute is the average number of events per minute.
	PerMinute float64
	// Burn returns the burn rate (multiple of the error budget) for traffic `age` before now.
	Burn func(age time.Duration) float64
	// Story is a one-line description of what the data should show.
	Story string
}

const healthyBurn = 0.3

// Scenarios are the demo services. All names describe a fictional company.
var Scenarios = []Scenario{
	{
		Service: "checkout-api", Team: "payments", SLO: "availability", Target: 0.999, WindowDays: 30, PerMinute: 1200,
		Description: "Fictional storefront checkout API",
		Burn:        func(time.Duration) float64 { return healthyBurn },
		Story:       "healthy: steady 0.3x burn, plenty of budget left",
	},
	{
		Service: "search-api", Team: "discovery", SLO: "latency-under-300ms", Target: 0.995, WindowDays: 30, PerMinute: 1800,
		Description: "Fictional product search API",
		Burn: func(age time.Duration) float64 {
			if age < 50*time.Minute {
				return 20
			}
			return healthyBurn
		},
		Story: "active incident: 20x burn for the last 50 minutes, fast-burn page",
	},
	{
		Service: "notifications-worker", Team: "messaging", SLO: "delivery", Target: 0.99, WindowDays: 30, PerMinute: 300,
		Description: "Fictional email and push delivery worker",
		Burn: func(age time.Duration) float64 {
			if age < 48*time.Hour {
				return 1.5
			}
			return healthyBurn
		},
		Story: "slow leak: 1.5x burn for two days, slow-burn ticket with an exhaustion forecast",
	},
	{
		Service: "billing-api", Team: "finance", SLO: "availability", Target: 0.9995, WindowDays: 30, PerMinute: 600,
		Description: "Fictional invoicing and billing API",
		Burn: func(age time.Duration) float64 {
			if age >= 240*time.Hour && age < 246*time.Hour {
				return 120
			}
			return healthyBurn
		},
		Story: "exhausted: a six-hour outage ten days ago used more than the whole budget",
	},
}

// Tokens are the demo API tokens created by Run. They exist only in the caller's memory.
type Tokens struct {
	Editor string
	Viewer string
}

// Run seeds an empty database with Scenarios ending at now and creates one editor and one
// viewer token. The traffic is generated from a fixed seed so every run looks the same.
func Run(ctx context.Context, st *store.Store, svc *service.Service, now time.Time) (Tokens, error) {
	existing, err := st.ListServices(ctx)
	if err != nil {
		return Tokens{}, err
	}
	if len(existing) > 0 {
		return Tokens{}, ErrAlreadySeeded
	}

	end := now.UTC().Truncate(time.Minute)
	// #nosec G404 -- deterministic synthetic demo traffic, not used for anything security-sensitive.
	rng := rand.New(rand.NewPCG(2026, 11))
	for _, sc := range Scenarios {
		if _, err := svc.CreateService(ctx, service.CreateServiceInput{Name: sc.Service, Team: sc.Team, Description: sc.Description}); err != nil {
			return Tokens{}, err
		}
		o, err := svc.CreateSLO(ctx, sc.Service, service.CreateSLOInput{Name: sc.SLO, Target: sc.Target, WindowDays: sc.WindowDays})
		if err != nil {
			return Tokens{}, err
		}
		if err := st.AddEvents(ctx, o.ID, Traffic(sc, end, rng)); err != nil {
			return Tokens{}, err
		}
	}

	var toks Tokens
	for _, t := range []struct {
		name string
		role model.Role
		dst  *string
	}{{"demo-editor", model.RoleEditor, &toks.Editor}, {"demo-viewer", model.RoleViewer, &toks.Viewer}} {
		plain, hash, err := auth.NewToken()
		if err != nil {
			return Tokens{}, err
		}
		if _, err := st.CreateToken(ctx, t.name, t.role, hash); err != nil {
			return Tokens{}, err
		}
		*t.dst = plain
	}
	return toks, nil
}

// Traffic generates one bucket per minute for the scenario's window, ending at end.
// Volume follows a daily cycle; failures follow the scenario's burn curve with noise.
func Traffic(sc Scenario, end time.Time, rng *rand.Rand) []slo.Bucket {
	budget := 1 - sc.Target
	window := time.Duration(sc.WindowDays) * 24 * time.Hour
	out := make([]slo.Bucket, 0, int(window/time.Minute))
	for t := end.Add(-window).Add(time.Minute); !t.After(end); t = t.Add(time.Minute) {
		hour := float64(t.Hour()) + float64(t.Minute())/60
		daily := 1 + 0.5*math.Sin(2*math.Pi*(hour-9)/24)
		total := stochasticRound(sc.PerMinute*daily*(0.9+0.2*rng.Float64()), rng)
		if total < 1 {
			total = 1
		}
		errRate := math.Min(1, sc.Burn(end.Sub(t))*budget*(0.7+0.6*rng.Float64()))
		bad := min(total, stochasticRound(float64(total)*errRate, rng))
		out = append(out, slo.Bucket{Start: t, Good: total - bad, Total: total})
	}
	return out
}

// stochasticRound rounds x up with probability equal to its fractional part, so small
// expected counts (e.g. 0.4 errors per minute) average out correctly instead of vanishing.
func stochasticRound(x float64, rng *rand.Rand) int64 {
	f := math.Floor(x)
	if rng.Float64() < x-f {
		f++
	}
	return int64(f)
}
