// Package slo computes error-budget and burn-rate figures for a service level objective.
//
// It implements the multi-window, multi-burn-rate alerting approach from the Google SRE
// Workbook ("Alerting on SLOs", chapter 5). An alert fires only when BOTH a long window
// (which proves the problem is significant) and a short window (which proves it is still
// happening) burn faster than the rule's threshold. That combination gives high precision
// (few false pages) with a fast reset once an incident is fixed.
//
// The package is pure: it has no I/O, so every figure can be verified in unit tests.
package slo

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

// Bucket is the number of good and total events observed in one time slice starting at Start.
type Bucket struct {
	Start time.Time
	Good  int64
	Total int64
}

// Objective is a reliability target over a rolling window, e.g. 99.9% over 30 days.
type Objective struct {
	Target float64
	Window time.Duration
}

// Validate reports whether the objective can produce meaningful figures.
func (o Objective) Validate() error {
	if !(o.Target > 0 && o.Target < 1) {
		return fmt.Errorf("target must be between 0 and 1 (exclusive), got %v", o.Target)
	}
	if o.Window < time.Hour {
		return errors.New("window must be at least one hour")
	}
	return nil
}

// ErrorBudget is the fraction of events allowed to fail, e.g. 0.001 for a 99.9% target.
func (o Objective) ErrorBudget() float64 { return 1 - o.Target }

// Severity says who should be told about a burn.
type Severity string

// Severities, ordered from least to most urgent.
const (
	SeverityNone   Severity = "none"
	SeverityTicket Severity = "ticket"
	SeverityPage   Severity = "page"
)

func (s Severity) rank() int {
	switch s {
	case SeverityPage:
		return 2
	case SeverityTicket:
		return 1
	default:
		return 0
	}
}

// Rank orders severities so callers can sort by urgency (page > ticket > none).
func (s Severity) Rank() int { return s.rank() }

// AlertRule fires when both windows burn faster than the rule's threshold.
type AlertRule struct {
	Name        string
	Severity    Severity
	LongWindow  time.Duration
	ShortWindow time.Duration
	// BudgetFraction is the share of the whole error budget that, if spent within
	// LongWindow, justifies this alert. It makes the rule independent of the SLO window.
	BudgetFraction float64
}

// Threshold is the burn rate at which this rule spends BudgetFraction of the budget
// within LongWindow, for an SLO measured over sloWindow.
//
// For a 30-day SLO this reproduces the SRE Workbook's familiar numbers:
// 2% in 1h -> 14.4, 5% in 6h -> 6, 10% in 3d -> 1.
func (r AlertRule) Threshold(sloWindow time.Duration) float64 {
	return r.BudgetFraction * sloWindow.Hours() / r.LongWindow.Hours()
}

// DefaultRules are the SRE Workbook's recommended multi-window, multi-burn-rate rules.
var DefaultRules = []AlertRule{
	{Name: "fast-burn", Severity: SeverityPage, LongWindow: time.Hour, ShortWindow: 5 * time.Minute, BudgetFraction: 0.02},
	{Name: "sustained-burn", Severity: SeverityPage, LongWindow: 6 * time.Hour, ShortWindow: 30 * time.Minute, BudgetFraction: 0.05},
	{Name: "slow-burn", Severity: SeverityTicket, LongWindow: 72 * time.Hour, ShortWindow: 6 * time.Hour, BudgetFraction: 0.10},
}

// LongestWindow returns the largest window that Compute needs data for.
func LongestWindow(o Objective, rules []AlertRule) time.Duration {
	longest := o.Window
	for _, r := range rules {
		longest = max(longest, r.LongWindow, r.ShortWindow)
	}
	return longest
}

// WindowBurn is the error rate and burn rate observed over one lookback window.
type WindowBurn struct {
	Window    string  `json:"window"`
	Good      int64   `json:"good"`
	Total     int64   `json:"total"`
	ErrorRate float64 `json:"error_rate"`
	// BurnRate is how many times faster than "exactly on budget" errors are occurring.
	// 1 means the budget would be used up precisely at the end of the SLO window.
	BurnRate float64 `json:"burn_rate"`

	duration time.Duration
}

// AlertResult is the evaluation of one AlertRule.
type AlertResult struct {
	Rule        string   `json:"rule"`
	Severity    Severity `json:"severity"`
	LongWindow  string   `json:"long_window"`
	ShortWindow string   `json:"short_window"`
	Threshold   float64  `json:"threshold"`
	LongBurn    float64  `json:"long_burn"`
	ShortBurn   float64  `json:"short_burn"`
	Firing      bool     `json:"firing"`
}

// Status is the complete error-budget picture for an objective at one instant.
type Status struct {
	At    time.Time `json:"at"`
	Good  int64     `json:"good"`
	Total int64     `json:"total"`
	// SLI is the observed good/total ratio over the SLO window; nil when there is no traffic.
	SLI             *float64 `json:"sli"`
	ErrorBudget     float64  `json:"error_budget"`
	BudgetConsumed  float64  `json:"budget_consumed"`
	BudgetRemaining float64  `json:"budget_remaining"`
	// Exhausted is true once the window's failures exceed the whole budget.
	Exhausted bool `json:"exhausted"`
	// HoursToExhaustion linearly projects when the remaining budget runs out at the
	// current sustained (6h) burn rate. It is nil when the budget is not on track to run out.
	HoursToExhaustion *float64      `json:"hours_to_exhaustion"`
	BurnRates         []WindowBurn  `json:"burn_rates"`
	Alerts            []AlertResult `json:"alerts"`
	Severity          Severity      `json:"severity"`
}

// projectionWindow is the burn window used for the time-to-exhaustion forecast: long
// enough to ignore short spikes, short enough to reflect the current trend.
const projectionWindow = 6 * time.Hour

// Compute evaluates obj at instant at from buckets (in any order). A bucket belongs to a
// window of length d when its start lies in (at-d, at].
func Compute(obj Objective, buckets []Bucket, at time.Time, rules []AlertRule) (Status, error) {
	if err := obj.Validate(); err != nil {
		return Status{}, err
	}
	budget := obj.ErrorBudget()

	// Collect every window we need, deduplicated and sorted shortest first.
	durations := []time.Duration{projectionWindow}
	for _, r := range rules {
		durations = append(durations, r.ShortWindow, r.LongWindow)
	}
	slices.Sort(durations)
	durations = slices.Compact(durations)

	burns := make([]WindowBurn, len(durations))
	byDuration := make(map[time.Duration]WindowBurn, len(durations))
	for i, d := range durations {
		good, total := sum(buckets, at, d)
		wb := WindowBurn{Window: FormatWindow(d), Good: good, Total: total, duration: d}
		if total > 0 {
			wb.ErrorRate = float64(total-good) / float64(total)
			wb.BurnRate = wb.ErrorRate / budget
		}
		burns[i] = wb
		byDuration[d] = wb
	}

	st := Status{At: at, ErrorBudget: budget, BurnRates: burns, Severity: SeverityNone}
	st.Good, st.Total = sum(buckets, at, obj.Window)
	if st.Total > 0 {
		sli := float64(st.Good) / float64(st.Total)
		st.SLI = &sli
		st.BudgetConsumed = (1 - sli) / budget
	}
	st.BudgetRemaining = 1 - st.BudgetConsumed
	st.Exhausted = st.BudgetConsumed >= 1

	if burn := byDuration[projectionWindow].BurnRate; !st.Exhausted && burn > 1 {
		// At burn rate b the budget is spent at b/windowHours of the budget per hour.
		hours := st.BudgetRemaining * obj.Window.Hours() / burn
		st.HoursToExhaustion = &hours
	}

	st.Alerts = make([]AlertResult, 0, len(rules))
	for _, r := range rules {
		threshold := r.Threshold(obj.Window)
		long, short := byDuration[r.LongWindow].BurnRate, byDuration[r.ShortWindow].BurnRate
		res := AlertResult{
			Rule:        r.Name,
			Severity:    r.Severity,
			LongWindow:  FormatWindow(r.LongWindow),
			ShortWindow: FormatWindow(r.ShortWindow),
			Threshold:   threshold,
			LongBurn:    long,
			ShortBurn:   short,
			Firing:      long > threshold && short > threshold,
		}
		if res.Firing && r.Severity.rank() > st.Severity.rank() {
			st.Severity = r.Severity
		}
		st.Alerts = append(st.Alerts, res)
	}
	return st, nil
}

func sum(buckets []Bucket, at time.Time, d time.Duration) (good, total int64) {
	from := at.Add(-d)
	for _, b := range buckets {
		if b.Start.After(from) && !b.Start.After(at) {
			good += b.Good
			total += b.Total
		}
	}
	return good, total
}

// FormatWindow renders a duration the way SREs write windows: 5m, 1h, 3d.
func FormatWindow(d time.Duration) string {
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}

// Round returns v rounded to the given number of decimal places, for display.
func Round(v float64, places int) float64 {
	p := math.Pow10(places)
	return math.Round(v*p) / p
}
