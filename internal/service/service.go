// Package service holds burnrate's use cases: input validation and orchestration between
// the repository and the pure SLO engine. It knows nothing about HTTP or SQL.
package service

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mbarney928/burnrate/internal/model"
	"github.com/Mbarney928/burnrate/internal/slo"
)

// Limits on client input.
const (
	MaxEventsPerRequest = 1000
	MaxEventsPerBucket  = 1_000_000_000
	MaxWindowDays       = 90
	MinTarget           = 0.5
	MaxTarget           = 0.99999
	// futureSkew tolerates clients whose clocks run slightly ahead.
	futureSkew = 5 * time.Minute
)

var slugRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

// Repository is the persistence the service depends on; *store.Store implements it.
type Repository interface {
	CreateService(ctx context.Context, svc model.Service) (model.Service, error)
	ServiceByName(ctx context.Context, name string) (model.Service, error)
	ListServices(ctx context.Context) ([]model.Service, error)
	CreateSLO(ctx context.Context, o model.SLO) (model.SLO, error)
	SLOByID(ctx context.Context, id int64) (model.SLO, error)
	ListSLOs(ctx context.Context, serviceID int64) ([]model.SLO, error)
	AddEvents(ctx context.Context, sloID int64, buckets []slo.Bucket) error
	Buckets(ctx context.Context, sloID int64, from, to time.Time) ([]slo.Bucket, error)
}

// Service implements burnrate's use cases.
type Service struct {
	repo  Repository
	now   func() time.Time
	rules []slo.AlertRule
}

// New returns a Service using the SRE Workbook's default alert rules. now is injectable
// for tests; pass nil to use time.Now.
func New(repo Repository, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{repo: repo, now: now, rules: slo.DefaultRules}
}

// CreateServiceInput is the payload for registering a service.
type CreateServiceInput struct {
	Name        string `json:"name"`
	Team        string `json:"team"`
	Description string `json:"description"`
}

// CreateService validates and stores a new service.
func (s *Service) CreateService(ctx context.Context, in CreateServiceInput) (model.Service, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Team = strings.TrimSpace(in.Team)
	in.Description = strings.TrimSpace(in.Description)

	var v model.ValidationError
	checkSlug(&v, "name", in.Name)
	checkText(&v, "team", in.Team, 1, 64)
	checkText(&v, "description", in.Description, 0, 500)
	if err := v.Err(); err != nil {
		return model.Service{}, err
	}
	return s.repo.CreateService(ctx, model.Service{Name: in.Name, Team: in.Team, Description: in.Description})
}

// GetService returns a service by name.
func (s *Service) GetService(ctx context.Context, name string) (model.Service, error) {
	return s.repo.ServiceByName(ctx, name)
}

// ListServices returns every service.
func (s *Service) ListServices(ctx context.Context) ([]model.Service, error) {
	return s.repo.ListServices(ctx)
}

// CreateSLOInput is the payload for adding an SLO to a service.
type CreateSLOInput struct {
	Name       string  `json:"name"`
	Target     float64 `json:"target"`
	WindowDays int     `json:"window_days"`
}

// CreateSLO validates and stores a new SLO for the named service.
func (s *Service) CreateSLO(ctx context.Context, serviceName string, in CreateSLOInput) (model.SLO, error) {
	in.Name = strings.TrimSpace(in.Name)
	var v model.ValidationError
	checkSlug(&v, "name", in.Name)
	if !(in.Target >= MinTarget && in.Target <= MaxTarget) {
		v.Add("target", fmt.Sprintf("must be between %v and %v, e.g. 0.999 for 99.9%%", MinTarget, MaxTarget))
	}
	if in.WindowDays < 1 || in.WindowDays > MaxWindowDays {
		v.Add("window_days", fmt.Sprintf("must be between 1 and %d", MaxWindowDays))
	}
	if err := v.Err(); err != nil {
		return model.SLO{}, err
	}
	svc, err := s.repo.ServiceByName(ctx, serviceName)
	if err != nil {
		return model.SLO{}, err
	}
	o, err := s.repo.CreateSLO(ctx, model.SLO{ServiceID: svc.ID, Name: in.Name, Target: in.Target, WindowDays: in.WindowDays})
	o.ServiceName = svc.Name
	return o, err
}

// ListSLOs returns the SLOs of the named service.
func (s *Service) ListSLOs(ctx context.Context, serviceName string) ([]model.SLO, error) {
	svc, err := s.repo.ServiceByName(ctx, serviceName)
	if err != nil {
		return nil, err
	}
	return s.repo.ListSLOs(ctx, svc.ID)
}

// GetSLO returns an SLO by ID.
func (s *Service) GetSLO(ctx context.Context, id int64) (model.SLO, error) {
	return s.repo.SLOByID(ctx, id)
}

// EventInput is a count of good and total events observed at Timestamp.
type EventInput struct {
	Timestamp time.Time `json:"timestamp"`
	Good      int64     `json:"good"`
	Total     int64     `json:"total"`
}

// RecordEvents validates a batch of event counts and adds them to the SLO's minute buckets.
// The batch is all-or-nothing: one invalid event rejects the whole request.
func (s *Service) RecordEvents(ctx context.Context, sloID int64, events []EventInput) (int, error) {
	o, err := s.repo.SLOByID(ctx, sloID)
	if err != nil {
		return 0, err
	}
	var v model.ValidationError
	if len(events) == 0 || len(events) > MaxEventsPerRequest {
		v.Add("events", fmt.Sprintf("must contain between 1 and %d items", MaxEventsPerRequest))
	}
	now := s.now()
	oldest := now.Add(-time.Duration(o.WindowDays) * 24 * time.Hour)
	buckets := make([]slo.Bucket, 0, len(events))
	for i, e := range events {
		field := fmt.Sprintf("events[%d]", i)
		switch {
		case e.Timestamp.IsZero():
			v.Add(field+".timestamp", "is required (RFC 3339)")
		case e.Timestamp.After(now.Add(futureSkew)):
			v.Add(field+".timestamp", "must not be in the future")
		case !e.Timestamp.After(oldest):
			v.Add(field+".timestamp", "is older than the SLO window")
		}
		if e.Total < 1 || e.Total > MaxEventsPerBucket {
			v.Add(field+".total", fmt.Sprintf("must be between 1 and %d", MaxEventsPerBucket))
		}
		if e.Good < 0 || e.Good > e.Total {
			v.Add(field+".good", "must be between 0 and total")
		}
		buckets = append(buckets, slo.Bucket{Start: e.Timestamp.UTC(), Good: e.Good, Total: e.Total})
	}
	if err := v.Err(); err != nil {
		return 0, err
	}
	if err := s.repo.AddEvents(ctx, sloID, buckets); err != nil {
		return 0, err
	}
	return len(buckets), nil
}

// SLOStatus pairs an SLO with its computed error-budget status.
type SLOStatus struct {
	SLO    model.SLO  `json:"slo"`
	Status slo.Status `json:"status"`
}

// Status computes the error-budget status of one SLO at instant at (zero means now).
func (s *Service) Status(ctx context.Context, sloID int64, at time.Time) (SLOStatus, error) {
	o, err := s.repo.SLOByID(ctx, sloID)
	if err != nil {
		return SLOStatus{}, err
	}
	return s.status(ctx, o, at)
}

// Overview computes the status of every SLO, most urgent first: pages, then tickets, then
// by least budget remaining.
func (s *Service) Overview(ctx context.Context, at time.Time) ([]SLOStatus, error) {
	all, err := s.repo.ListSLOs(ctx, 0)
	if err != nil {
		return nil, err
	}
	out := make([]SLOStatus, 0, len(all))
	for _, o := range all {
		st, err := s.status(ctx, o, at)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	slices.SortStableFunc(out, func(a, b SLOStatus) int {
		if ra, rb := a.Status.Severity.Rank(), b.Status.Severity.Rank(); ra != rb {
			return rb - ra
		}
		switch {
		case a.Status.BudgetRemaining < b.Status.BudgetRemaining:
			return -1
		case a.Status.BudgetRemaining > b.Status.BudgetRemaining:
			return 1
		}
		return 0
	})
	return out, nil
}

func (s *Service) status(ctx context.Context, o model.SLO, at time.Time) (SLOStatus, error) {
	if at.IsZero() {
		at = s.now()
	}
	at = at.UTC()
	obj := slo.Objective{Target: o.Target, Window: time.Duration(o.WindowDays) * 24 * time.Hour}
	buckets, err := s.repo.Buckets(ctx, o.ID, at.Add(-slo.LongestWindow(obj, s.rules)), at)
	if err != nil {
		return SLOStatus{}, err
	}
	st, err := slo.Compute(obj, buckets, at, s.rules)
	if err != nil {
		return SLOStatus{}, err
	}
	return SLOStatus{SLO: o, Status: st}, nil
}

func checkSlug(v *model.ValidationError, field, val string) {
	if !slugRE.MatchString(val) {
		v.Add(field, "must be 2-63 characters of lowercase letters, digits and dashes, starting with a letter")
	}
}

func checkText(v *model.ValidationError, field, val string, minLen, maxLen int) {
	n := utf8.RuneCountInString(val)
	if n < minLen || n > maxLen {
		v.Add(field, fmt.Sprintf("must be between %d and %d characters", minLen, maxLen))
	}
}
