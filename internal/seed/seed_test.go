package seed

import (
	"context"
	"errors"
	"math/rand/v2"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mbarney928/burnrate/internal/auth"
	"github.com/Mbarney928/burnrate/internal/model"
	"github.com/Mbarney928/burnrate/internal/service"
	"github.com/Mbarney928/burnrate/internal/slo"
	"github.com/Mbarney928/burnrate/internal/store"
)

var now = time.Date(2026, 3, 10, 12, 0, 30, 0, time.UTC)

func TestSeedTellsEachStory(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, func() time.Time { return now })

	toks, err := Run(ctx, st, svc, now)
	if err != nil {
		t.Fatal(err)
	}
	for plain, role := range map[string]model.Role{toks.Editor: model.RoleEditor, toks.Viewer: model.RoleViewer} {
		tok, err := st.TokenByHash(ctx, auth.Hash(plain))
		if err != nil || tok.Role != role {
			t.Errorf("token for %s: %+v, %v", role, tok, err)
		}
	}
	if _, err := Run(ctx, st, svc, now); !errors.Is(err, ErrAlreadySeeded) {
		t.Errorf("second seed err = %v, want ErrAlreadySeeded", err)
	}

	all, err := svc.Overview(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]slo.Status{}
	for _, s := range all {
		got[s.SLO.ServiceName] = s.Status
	}
	if s := got["checkout-api"]; s.Severity != slo.SeverityNone || s.BudgetRemaining < 0.5 || s.Exhausted {
		t.Errorf("checkout-api should be healthy: severity %s, remaining %.2f", s.Severity, s.BudgetRemaining)
	}
	if s := got["search-api"]; s.Severity != slo.SeverityPage {
		t.Errorf("search-api should page, got %s", s.Severity)
	}
	if s := got["notifications-worker"]; s.Severity != slo.SeverityTicket || s.HoursToExhaustion == nil {
		t.Errorf("notifications-worker should ticket with a forecast, got %s / %v", s.Severity, s.HoursToExhaustion)
	}
	if s := got["billing-api"]; !s.Exhausted || s.Severity != slo.SeverityNone {
		t.Errorf("billing-api should be exhausted without a live alert, got exhausted=%v severity=%s", s.Exhausted, s.Severity)
	}
	if all[0].SLO.ServiceName != "search-api" {
		t.Errorf("overview should list the page first, got %s", all[0].SLO.ServiceName)
	}
}

func TestTrafficIsDeterministicAndValid(t *testing.T) {
	sc := Scenarios[0]
	a := Traffic(sc, now, rand.New(rand.NewPCG(1, 2)))
	b := Traffic(sc, now, rand.New(rand.NewPCG(1, 2)))
	if len(a) != 30*24*60 || len(a) != len(b) {
		t.Fatalf("expected one bucket per minute for 30 days, got %d", len(a))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("traffic must be deterministic for a given seed")
		}
		if a[i].Total < 1 || a[i].Good < 0 || a[i].Good > a[i].Total {
			t.Fatalf("invalid bucket %+v", a[i])
		}
	}
}

func TestStochasticRoundAveragesCorrectly(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	var sum int64
	for range 100000 {
		sum += stochasticRound(0.25, rng)
	}
	if avg := float64(sum) / 100000; avg < 0.24 || avg > 0.26 {
		t.Errorf("average = %v, want ~0.25", avg)
	}
}
