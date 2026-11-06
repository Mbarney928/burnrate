package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mbarney928/burnrate/internal/model"
	"github.com/Mbarney928/burnrate/internal/slo"
)

var fixed = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.now = func() time.Time { return fixed }
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := newStore(t)
	n, err := s.Migrate(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("second Migrate = %d, %v; want 0, nil", n, err)
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestOpenFailsForUnwritablePath(t *testing.T) {
	if _, err := Open(context.Background(), filepath.Join(t.TempDir(), "missing-dir", "x.db")); err == nil {
		t.Fatal("expected an error for a path in a missing directory")
	}
}

func TestServicesAndSLOs(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	svc, err := s.CreateService(ctx, model.Service{Name: "checkout-api", Team: "payments", Description: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if svc.ID == 0 || !svc.CreatedAt.Equal(fixed) {
		t.Errorf("unexpected service %+v", svc)
	}
	if _, err := s.CreateService(ctx, model.Service{Name: "checkout-api", Team: "x"}); !errors.Is(err, model.ErrConflict) {
		t.Errorf("duplicate service err = %v, want ErrConflict", err)
	}
	if _, err := s.ServiceByName(ctx, "nope"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("missing service err = %v, want ErrNotFound", err)
	}
	got, err := s.ServiceByName(ctx, "checkout-api")
	if err != nil || got != svc {
		t.Errorf("ServiceByName = %+v, %v; want %+v", got, err, svc)
	}

	other, err := s.CreateService(ctx, model.Service{Name: "billing-api", Team: "finance"})
	if err != nil {
		t.Fatal(err)
	}
	all, err := s.ListServices(ctx)
	if err != nil || len(all) != 2 || all[0].Name != "billing-api" {
		t.Errorf("ListServices = %+v, %v", all, err)
	}

	o, err := s.CreateSLO(ctx, model.SLO{ServiceID: svc.ID, Name: "availability", Target: 0.999, WindowDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSLO(ctx, model.SLO{ServiceID: svc.ID, Name: "availability", Target: 0.99, WindowDays: 7}); !errors.Is(err, model.ErrConflict) {
		t.Errorf("duplicate SLO err = %v, want ErrConflict", err)
	}
	if _, err := s.CreateSLO(ctx, model.SLO{ServiceID: 9999, Name: "x", Target: 0.99, WindowDays: 7}); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("SLO for missing service err = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateSLO(ctx, model.SLO{ServiceID: other.ID, Name: "latency", Target: 0.99, WindowDays: 7}); err != nil {
		t.Fatal(err)
	}

	byID, err := s.SLOByID(ctx, o.ID)
	if err != nil || byID.ServiceName != "checkout-api" || byID.Target != 0.999 || byID.WindowDays != 30 {
		t.Errorf("SLOByID = %+v, %v", byID, err)
	}
	if _, err := s.SLOByID(ctx, 12345); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("missing SLO err = %v", err)
	}
	if list, err := s.ListSLOs(ctx, svc.ID); err != nil || len(list) != 1 {
		t.Errorf("ListSLOs(service) = %+v, %v", list, err)
	}
	if list, err := s.ListSLOs(ctx, 0); err != nil || len(list) != 2 || list[0].ServiceName != "billing-api" {
		t.Errorf("ListSLOs(all) = %+v, %v", list, err)
	}
}

func TestEventsAccumulatePerMinute(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	svc, _ := s.CreateService(ctx, model.Service{Name: "search-api", Team: "discovery"})
	o, _ := s.CreateSLO(ctx, model.SLO{ServiceID: svc.ID, Name: "availability", Target: 0.99, WindowDays: 7})

	m := fixed.Add(-10 * time.Minute)
	err := s.AddEvents(ctx, o.ID, []slo.Bucket{
		{Start: m.Add(15 * time.Second), Good: 90, Total: 100},
		{Start: m.Add(40 * time.Second), Good: 5, Total: 10}, // same minute: adds up
		{Start: m.Add(time.Minute), Good: 1, Total: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Buckets(ctx, o.ID, fixed.Add(-time.Hour), fixed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Good != 95 || got[0].Total != 110 || !got[0].Start.Equal(m) {
		t.Fatalf("Buckets = %+v", got)
	}

	// The CHECK constraint rejects impossible counts and the whole batch rolls back.
	err = s.AddEvents(ctx, o.ID, []slo.Bucket{{Start: fixed, Good: 1, Total: 1}, {Start: fixed, Good: 5, Total: 1}})
	if err == nil {
		t.Fatal("expected a constraint error")
	}
	if got, _ := s.Buckets(ctx, o.ID, fixed.Add(-time.Hour), fixed); len(got) != 2 {
		t.Errorf("partial batch was committed: %+v", got)
	}
}

func TestTokens(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	tok, err := s.CreateToken(ctx, "ci", model.RoleEditor, "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken(ctx, "ci", model.RoleViewer, "hash-2"); !errors.Is(err, model.ErrConflict) {
		t.Errorf("duplicate token name err = %v", err)
	}
	got, err := s.TokenByHash(ctx, "hash-1")
	if err != nil || got != tok || got.Role != model.RoleEditor {
		t.Errorf("TokenByHash = %+v, %v; want %+v", got, err, tok)
	}
	if _, err := s.TokenByHash(ctx, "unknown"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown token err = %v", err)
	}
}

func TestQueriesFailAfterClose(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	_ = s.Close()
	if _, err := s.ListServices(ctx); err == nil {
		t.Error("ListServices on a closed store should fail")
	}
	if _, err := s.ListSLOs(ctx, 0); err == nil {
		t.Error("ListSLOs on a closed store should fail")
	}
	if _, err := s.Buckets(ctx, 1, fixed, fixed); err == nil {
		t.Error("Buckets on a closed store should fail")
	}
	if err := s.AddEvents(ctx, 1, nil); err == nil {
		t.Error("AddEvents on a closed store should fail")
	}
	if _, err := s.Migrate(ctx); err == nil {
		t.Error("Migrate on a closed store should fail")
	}
}

func TestModelHelpers(t *testing.T) {
	if !model.RoleEditor.CanWrite() || model.RoleViewer.CanWrite() || model.Role("admin").Valid() || !model.RoleViewer.Valid() {
		t.Error("role helpers are wrong")
	}
	var v model.ValidationError
	if v.Err() != nil {
		t.Error("empty ValidationError should be nil")
	}
	v.Add("name", "first")
	v.Add("name", "second")
	if v.Fields["name"] != "first" || v.Err() == nil || v.Error() != "validation failed" {
		t.Errorf("ValidationError = %+v", v)
	}
}
