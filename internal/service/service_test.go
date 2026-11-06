package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mbarney928/burnrate/internal/model"
	"github.com/Mbarney928/burnrate/internal/slo"
	"github.com/Mbarney928/burnrate/internal/store"
)

var now = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func newService(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "svc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return New(st, func() time.Time { return now })
}

func fieldsOf(t *testing.T, err error) map[string]string {
	t.Helper()
	var v *model.ValidationError
	if !errors.As(err, &v) {
		t.Fatalf("expected a ValidationError, got %v", err)
	}
	return v.Fields
}

func TestCreateServiceValidation(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	_, err := s.CreateService(ctx, CreateServiceInput{Name: "Bad Name!", Team: "", Description: strings.Repeat("x", 501)})
	f := fieldsOf(t, err)
	for _, k := range []string{"name", "team", "description"} {
		if f[k] == "" {
			t.Errorf("expected a %s error, got %v", k, f)
		}
	}

	svc, err := s.CreateService(ctx, CreateServiceInput{Name: "  checkout-api ", Team: " payments ", Description: "Fictional store checkout"})
	if err != nil || svc.Name != "checkout-api" || svc.Team != "payments" {
		t.Fatalf("CreateService = %+v, %v", svc, err)
	}
	if _, err := s.CreateService(ctx, CreateServiceInput{Name: "checkout-api", Team: "x"}); !errors.Is(err, model.ErrConflict) {
		t.Errorf("duplicate err = %v", err)
	}
	if got, err := s.GetService(ctx, "checkout-api"); err != nil || got.ID != svc.ID {
		t.Errorf("GetService = %+v, %v", got, err)
	}
	if list, err := s.ListServices(ctx); err != nil || len(list) != 1 {
		t.Errorf("ListServices = %+v, %v", list, err)
	}
}

func TestCreateSLOValidation(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	if _, err := s.CreateService(ctx, CreateServiceInput{Name: "search-api", Team: "discovery"}); err != nil {
		t.Fatal(err)
	}
	f := fieldsOf(t, func() error {
		_, err := s.CreateSLO(ctx, "search-api", CreateSLOInput{Name: "x", Target: 1, WindowDays: 91})
		return err
	}())
	for _, k := range []string{"name", "target", "window_days"} {
		if f[k] == "" {
			t.Errorf("expected a %s error, got %v", k, f)
		}
	}
	if _, err := s.CreateSLO(ctx, "missing", CreateSLOInput{Name: "availability", Target: 0.99, WindowDays: 30}); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown service err = %v", err)
	}
	o, err := s.CreateSLO(ctx, "search-api", CreateSLOInput{Name: "availability", Target: 0.995, WindowDays: 28})
	if err != nil || o.ServiceName != "search-api" || o.ID == 0 {
		t.Fatalf("CreateSLO = %+v, %v", o, err)
	}
	if list, err := s.ListSLOs(ctx, "search-api"); err != nil || len(list) != 1 {
		t.Errorf("ListSLOs = %+v, %v", list, err)
	}
	if _, err := s.ListSLOs(ctx, "missing"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("ListSLOs(missing) err = %v", err)
	}
	if got, err := s.GetSLO(ctx, o.ID); err != nil || got.Name != "availability" {
		t.Errorf("GetSLO = %+v, %v", got, err)
	}
}

func setupSLO(t *testing.T, s *Service, name string, target float64) model.SLO {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateService(ctx, CreateServiceInput{Name: name, Team: "ops"}); err != nil {
		t.Fatal(err)
	}
	o, err := s.CreateSLO(ctx, name, CreateSLOInput{Name: "availability", Target: target, WindowDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestRecordEventsValidation(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	o := setupSLO(t, s, "billing-api", 0.999)

	if _, err := s.RecordEvents(ctx, 999, []EventInput{{Timestamp: now, Good: 1, Total: 1}}); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown SLO err = %v", err)
	}
	if f := fieldsOf(t, func() error { _, err := s.RecordEvents(ctx, o.ID, nil); return err }()); f["events"] == "" {
		t.Errorf("empty batch should be rejected: %v", f)
	}
	tooMany := make([]EventInput, MaxEventsPerRequest+1)
	if f := fieldsOf(t, func() error { _, err := s.RecordEvents(ctx, o.ID, tooMany); return err }()); f["events"] == "" {
		t.Errorf("oversized batch should be rejected: %v", f)
	}

	_, err := s.RecordEvents(ctx, o.ID, []EventInput{
		{Timestamp: time.Time{}, Good: 1, Total: 1},
		{Timestamp: now.Add(time.Hour), Good: 1, Total: 1},
		{Timestamp: now.Add(-31 * 24 * time.Hour), Good: 1, Total: 1},
		{Timestamp: now, Good: 1, Total: 0},
		{Timestamp: now, Good: 5, Total: 4},
		{Timestamp: now, Good: -1, Total: 4},
	})
	f := fieldsOf(t, err)
	for _, k := range []string{"events[0].timestamp", "events[1].timestamp", "events[2].timestamp", "events[3].total", "events[4].good", "events[5].good"} {
		if f[k] == "" {
			t.Errorf("expected an error for %s, got %v", k, f)
		}
	}

	n, err := s.RecordEvents(ctx, o.ID, []EventInput{
		{Timestamp: now.Add(2 * time.Minute), Good: 10, Total: 10}, // within clock-skew tolerance
		{Timestamp: now.Add(-time.Minute), Good: 9, Total: 10},
	})
	if err != nil || n != 2 {
		t.Fatalf("RecordEvents = %d, %v", n, err)
	}
}

func TestStatusAndOverviewOrdering(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	healthy := setupSLO(t, s, "checkout-api", 0.999)
	burning := setupSLO(t, s, "search-api", 0.99)
	_ = setupSLO(t, s, "quiet-api", 0.99) // no traffic at all

	var good, bad []EventInput
	for m := range 60 {
		ts := now.Add(-time.Duration(m) * time.Minute)
		good = append(good, EventInput{Timestamp: ts, Good: 1000, Total: 1000})
		bad = append(bad, EventInput{Timestamp: ts, Good: 700, Total: 1000}) // 30% errors: burn 30
	}
	if _, err := s.RecordEvents(ctx, healthy.ID, good); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordEvents(ctx, burning.ID, bad); err != nil {
		t.Fatal(err)
	}

	st, err := s.Status(ctx, burning.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status.Severity != slo.SeverityPage || !st.Status.At.Equal(now) {
		t.Errorf("burning SLO status = %+v", st.Status)
	}
	if _, err := s.Status(ctx, 12345, now); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown SLO err = %v", err)
	}

	all, err := s.Overview(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("Overview returned %d SLOs", len(all))
	}
	if all[0].SLO.ServiceName != "search-api" {
		t.Errorf("most urgent first: got %s", all[0].SLO.ServiceName)
	}
	if all[1].Status.BudgetRemaining > all[2].Status.BudgetRemaining {
		t.Errorf("non-alerting SLOs should be ordered by remaining budget: %v then %v",
			all[1].Status.BudgetRemaining, all[2].Status.BudgetRemaining)
	}
}

type failingRepo struct{ Repository }

func (failingRepo) ListSLOs(context.Context, int64) ([]model.SLO, error) {
	return []model.SLO{{ID: 1, Target: 0.99, WindowDays: 30}}, nil
}

func (failingRepo) Buckets(context.Context, int64, time.Time, time.Time) ([]slo.Bucket, error) {
	return nil, errors.New("disk on fire")
}

func TestOverviewPropagatesRepositoryErrors(t *testing.T) {
	s := New(failingRepo{}, nil)
	if _, err := s.Overview(context.Background(), time.Time{}); err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("Overview err = %v", err)
	}
}

type badSLORepo struct{ Repository }

func (badSLORepo) SLOByID(context.Context, int64) (model.SLO, error) {
	return model.SLO{ID: 1, Target: 1.5, WindowDays: 30}, nil
}

func (badSLORepo) Buckets(context.Context, int64, time.Time, time.Time) ([]slo.Bucket, error) {
	return nil, nil
}

func TestStatusRejectsCorruptObjective(t *testing.T) {
	s := New(badSLORepo{}, nil)
	if _, err := s.Status(context.Background(), 1, time.Time{}); err == nil {
		t.Error("expected an error for an impossible stored target")
	}
}
