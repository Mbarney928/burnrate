package slo

import (
	"math"
	"testing"
	"time"
)

var now = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

// steady returns one bucket per minute over d ending at now, each with total events and
// errorRate of them bad (rounded per bucket).
func steady(d time.Duration, total int64, errorRate float64, end time.Time) []Bucket {
	var out []Bucket
	for t := end.Add(-d).Add(time.Minute); !t.After(end); t = t.Add(time.Minute) {
		bad := int64(math.Round(float64(total) * errorRate))
		out = append(out, Bucket{Start: t, Good: total - bad, Total: total})
	}
	return out
}

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9*math.Max(1, math.Abs(want)) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func alertByName(t *testing.T, st Status, name string) AlertResult {
	t.Helper()
	for _, a := range st.Alerts {
		if a.Rule == name {
			return a
		}
	}
	t.Fatalf("alert %q not found", name)
	return AlertResult{}
}

func TestThresholdsMatchSREWorkbookFor30Days(t *testing.T) {
	want := map[string]float64{"fast-burn": 14.4, "sustained-burn": 6, "slow-burn": 1}
	for _, r := range DefaultRules {
		approx(t, r.Name, r.Threshold(30*day), want[r.Name])
	}
}

func TestThresholdsScaleWithWindow(t *testing.T) {
	// A 7-day SLO has a smaller budget in absolute time, so the same budget share
	// is spent at a lower burn rate.
	approx(t, "fast-burn@7d", DefaultRules[0].Threshold(7*day), 0.02*168)
}

func TestObjectiveValidate(t *testing.T) {
	for _, o := range []Objective{
		{Target: 0, Window: day},
		{Target: 1, Window: day},
		{Target: math.NaN(), Window: day},
		{Target: 0.99, Window: 30 * time.Minute},
	} {
		if o.Validate() == nil {
			t.Errorf("expected %+v to be invalid", o)
		}
		if _, err := Compute(o, nil, now, DefaultRules); err == nil {
			t.Errorf("Compute accepted invalid objective %+v", o)
		}
	}
}

func TestNoTrafficIsHealthy(t *testing.T) {
	st, err := Compute(Objective{Target: 0.999, Window: 30 * day}, nil, now, DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	if st.SLI != nil || st.BudgetRemaining != 1 || st.Exhausted || st.Severity != SeverityNone || st.HoursToExhaustion != nil {
		t.Errorf("unexpected status for no traffic: %+v", st)
	}
	for _, b := range st.BurnRates {
		if b.BurnRate != 0 || b.ErrorRate != 0 {
			t.Errorf("window %s should be zero, got %+v", b.Window, b)
		}
	}
}

func TestOnBudgetBurnsAtExactlyOne(t *testing.T) {
	obj := Objective{Target: 0.99, Window: 3 * day}
	st, err := Compute(obj, steady(3*day, 1000, 0.01, now), now, DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "SLI", *st.SLI, 0.99)
	approx(t, "BudgetConsumed", st.BudgetConsumed, 1)
	if !st.Exhausted {
		t.Error("spending exactly the whole budget should count as exhausted")
	}
	for _, b := range st.BurnRates {
		approx(t, "burn "+b.Window, b.BurnRate, 1)
	}
}

func TestFastBurnPages(t *testing.T) {
	obj := Objective{Target: 0.999, Window: 30 * day}
	// 30 days at 30% of the budget (burn 0.3), then the last 30 minutes at 2% errors (burn 20).
	buckets := steady(30*day, 10000, 0.0003, now.Add(-30*time.Minute))
	buckets = append(buckets, steady(30*time.Minute, 10000, 0.02, now)...)

	st, err := Compute(obj, buckets, now, DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	fast := alertByName(t, st, "fast-burn")
	// 1h window: 30 min at burn 0.3 and 30 min at burn 20 -> 10.15, which is below 14.4.
	approx(t, "fast long burn", fast.LongBurn, (0.3+20)/2)
	if fast.Firing {
		t.Error("fast-burn should not fire until the 1h window confirms the problem")
	}

	// Twenty minutes later 50 of the last 60 minutes are incident traffic: burn ~16.7.
	later := now.Add(20 * time.Minute)
	buckets = append(buckets, steady(20*time.Minute, 10000, 0.02, later)...)
	st, err = Compute(obj, buckets, later, DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	fast = alertByName(t, st, "fast-burn")
	if !fast.Firing || st.Severity != SeverityPage {
		t.Errorf("expected a page, got severity %s and %+v", st.Severity, fast)
	}
	if alertByName(t, st, "slow-burn").Firing {
		t.Error("slow-burn should not fire: the 3d window is still mostly healthy")
	}
}

func TestShortWindowResetsAlertAfterRecovery(t *testing.T) {
	obj := Objective{Target: 0.999, Window: 30 * day}
	// A 50-minute incident that ended 10 minutes ago.
	buckets := steady(30*day, 1000, 0, now.Add(-time.Hour))
	buckets = append(buckets, steady(50*time.Minute, 1000, 0.05, now.Add(-10*time.Minute))...)
	buckets = append(buckets, steady(10*time.Minute, 1000, 0, now)...)

	st, err := Compute(obj, buckets, now, DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	fast := alertByName(t, st, "fast-burn")
	if fast.LongBurn <= fast.Threshold {
		t.Fatalf("test setup: 1h burn %v should still exceed %v", fast.LongBurn, fast.Threshold)
	}
	if fast.Firing {
		t.Error("the 5m short window should stop the page once errors have stopped")
	}
}

func TestSlowBurnTicketsAndForecastsExhaustion(t *testing.T) {
	obj := Objective{Target: 0.99, Window: 30 * day}
	// 28 days at 30% of budget, then 2 days at 1.5x budget.
	buckets := steady(28*day, 1000, 0.003, now.Add(-2*day))
	buckets = append(buckets, steady(2*day, 1000, 0.015, now)...)

	st, err := Compute(obj, buckets, now, DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	if st.Severity != SeverityTicket {
		t.Fatalf("severity = %s, want ticket", st.Severity)
	}
	consumed := (28*0.3 + 2*1.5) / 30
	approx(t, "BudgetConsumed", st.BudgetConsumed, consumed)
	if st.HoursToExhaustion == nil {
		t.Fatal("expected an exhaustion forecast while burning faster than budget")
	}
	approx(t, "HoursToExhaustion", *st.HoursToExhaustion, (1-consumed)*720/1.5)
}

func TestExhaustedBudgetHasNoForecast(t *testing.T) {
	obj := Objective{Target: 0.99, Window: day}
	st, err := Compute(obj, steady(day, 100, 0.05, now), now, DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Exhausted || st.BudgetRemaining >= 0 || st.HoursToExhaustion != nil {
		t.Errorf("expected exhausted with no forecast, got %+v", st)
	}
}

func TestWindowBoundariesAreHalfOpen(t *testing.T) {
	obj := Objective{Target: 0.9, Window: time.Hour}
	buckets := []Bucket{
		{Start: now.Add(-time.Hour), Good: 0, Total: 100}, // exactly at the lower bound: excluded
		{Start: now, Good: 10, Total: 10},                 // exactly at "at": included
		{Start: now.Add(time.Minute), Good: 0, Total: 50}, // in the future: excluded
	}
	st, err := Compute(obj, buckets, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 10 || st.Good != 10 {
		t.Errorf("window sum = %d/%d, want 10/10", st.Good, st.Total)
	}
}

func TestLongestWindow(t *testing.T) {
	if got := LongestWindow(Objective{Target: 0.99, Window: day}, DefaultRules); got != 72*time.Hour {
		t.Errorf("LongestWindow = %v, want 72h", got)
	}
	if got := LongestWindow(Objective{Target: 0.99, Window: 30 * day}, DefaultRules); got != 30*day {
		t.Errorf("LongestWindow = %v, want 720h", got)
	}
}

func TestFormatWindowAndRound(t *testing.T) {
	cases := map[time.Duration]string{
		5 * time.Minute: "5m", time.Hour: "1h", 6 * time.Hour: "6h", 72 * time.Hour: "3d",
		90 * time.Minute: "90m", 1500 * time.Millisecond: "1.5s",
	}
	for d, want := range cases {
		if got := FormatWindow(d); got != want {
			t.Errorf("FormatWindow(%v) = %q, want %q", d, got, want)
		}
	}
	if Round(0.123456, 3) != 0.123 {
		t.Errorf("Round = %v", Round(0.123456, 3))
	}
	if SeverityPage.Rank() <= SeverityTicket.Rank() || SeverityTicket.Rank() <= SeverityNone.Rank() {
		t.Error("severity ranks out of order")
	}
}
