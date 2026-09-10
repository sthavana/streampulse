package alert

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("tzdata unavailable for %s: %v", name, err)
	}
	return loc
}

func daily(t *testing.T, start, end string, days ...time.Weekday) *Daily {
	t.Helper()
	s, err := ParseClock(start)
	if err != nil {
		t.Fatal(err)
	}
	e, err := ParseClock(end)
	if err != nil {
		t.Fatal(err)
	}
	return &Daily{StartMin: s, EndMin: e, Days: days}
}

// --- one-off windows ---

func TestOneOffWindowBoundsAreHalfOpen(t *testing.T) {
	start := time.Date(2026, 9, 15, 22, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 16, 2, 0, 0, 0, time.UTC)
	w := Window{Name: "packager upgrade", Start: start, End: end}

	cases := []struct {
		at   time.Time
		want bool
	}{
		{start.Add(-time.Second), false},
		{start, true}, // start is inclusive
		{start.Add(2 * time.Hour), true},
		{end.Add(-time.Second), true},
		{end, false}, // end is exclusive
		{end.Add(time.Second), false},
	}
	for _, c := range cases {
		if got := w.Active(c.at); got != c.want {
			t.Errorf("Active(%s) = %v, want %v", c.at.Format(time.RFC3339), got, c.want)
		}
	}
}

// --- daily windows ---

func TestDailyWindowWithinDay(t *testing.T) {
	w := Window{Name: "nightly", Loc: time.UTC, Daily: daily(t, "02:00", "04:00")}

	at := func(h, m int) time.Time { return time.Date(2026, 9, 15, h, m, 0, 0, time.UTC) }
	cases := []struct {
		t    time.Time
		want bool
	}{
		{at(1, 59), false},
		{at(2, 0), true},
		{at(3, 30), true},
		{at(3, 59), true},
		{at(4, 0), false},
		{at(12, 0), false},
	}
	for _, c := range cases {
		if got := w.Active(c.t); got != c.want {
			t.Errorf("Active(%s) = %v, want %v", c.t.Format("15:04"), got, c.want)
		}
	}
}

func TestDailyWindowCrossingMidnight(t *testing.T) {
	w := Window{Name: "overnight", Loc: time.UTC, Daily: daily(t, "22:00", "02:00")}

	at := func(d, h int) time.Time { return time.Date(2026, 9, d, h, 0, 0, 0, time.UTC) }
	cases := []struct {
		t    time.Time
		want bool
	}{
		{at(15, 21), false},
		{at(15, 22), true}, // opens
		{at(15, 23), true},
		{at(16, 0), true}, // past midnight, still open
		{at(16, 1), true},
		{at(16, 2), false}, // closes
		{at(16, 12), false},
	}
	for _, c := range cases {
		if got := w.Active(c.t); got != c.want {
			t.Errorf("Active(%s) = %v, want %v", c.t.Format("Jan 2 15:04"), got, c.want)
		}
	}
}

// A midnight-crossing window is gated by the weekday it OPENED on, so the small
// hours belong to the previous day. A Saturday-night window covers Sunday 01:00.
func TestMidnightCrossingWindowUsesOpeningWeekday(t *testing.T) {
	w := Window{Name: "sat night", Loc: time.UTC, Daily: daily(t, "22:00", "02:00", time.Saturday)}

	sat := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC) // a Saturday
	if sat.Weekday() != time.Saturday {
		t.Fatalf("fixture wrong: %s is %s", sat.Format("2006-01-02"), sat.Weekday())
	}

	cases := []struct {
		t    time.Time
		want bool
		why  string
	}{
		{sat.Add(23 * time.Hour), true, "Saturday 23:00, inside"},
		{sat.Add(25 * time.Hour), true, "Sunday 01:00, window opened Saturday"},
		{sat.Add(27 * time.Hour), false, "Sunday 03:00, window closed"},
		{sat.Add(47 * time.Hour), false, "Sunday 23:00, window opens Saturdays only"},
		{sat.Add(49 * time.Hour), false, "Monday 01:00, previous day was Sunday"},
	}
	for _, c := range cases {
		if got := w.Active(c.t); got != c.want {
			t.Errorf("%s: Active = %v, want %v", c.why, got, c.want)
		}
	}
}

func TestDailyWindowRespectsTimezone(t *testing.T) {
	la := mustLoc(t, "America/Los_Angeles")
	w := Window{Name: "station overnight", Loc: la, Daily: daily(t, "02:00", "04:00")}

	// 03:00 Los Angeles on 2026-09-15 is 10:00 UTC (PDT, UTC-7).
	inside := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	if !w.Active(inside) {
		t.Errorf("expected active at %s (03:00 LA)", inside.Format(time.RFC3339))
	}
	// 03:00 UTC is 20:00 the previous evening in LA: outside.
	outside := time.Date(2026, 9, 15, 3, 0, 0, 0, time.UTC)
	if w.Active(outside) {
		t.Errorf("expected inactive at %s (20:00 LA)", outside.Format(time.RFC3339))
	}
}

// Maintenance is scheduled in station-local wall-clock time, so a window must
// stay pinned to 02:00 local across a DST transition rather than drift by an hour.
func TestDailyWindowHoldsLocalTimeAcrossDST(t *testing.T) {
	la := mustLoc(t, "America/Los_Angeles")
	w := Window{Name: "nightly", Loc: la, Daily: daily(t, "02:00", "04:00")}

	// US DST ended 2026-11-01. Check a day either side, at 03:00 local.
	before := time.Date(2026, 10, 15, 3, 0, 0, 0, la) // PDT
	after := time.Date(2026, 11, 15, 3, 0, 0, 0, la)  // PST

	if !w.Active(before) {
		t.Error("window should be open at 03:00 local before the DST change")
	}
	if !w.Active(after) {
		t.Error("window should still be open at 03:00 local after the DST change")
	}
	// The same instants in UTC differ by an hour, proving it is not UTC-pinned.
	if before.UTC().Hour() == after.UTC().Hour() {
		t.Fatal("fixture wrong: DST transition did not shift the UTC offset")
	}
}

func TestNilLocationDefaultsToUTC(t *testing.T) {
	w := Window{Name: "no tz", Daily: daily(t, "02:00", "04:00")}
	if !w.Active(time.Date(2026, 9, 15, 3, 0, 0, 0, time.UTC)) {
		t.Error("a window with no location should be interpreted as UTC")
	}
}

// --- scope matching ---

func TestWindowScopeMatching(t *testing.T) {
	f := Finding{Target: "chan1", Check: "playlist_stalled"}

	cases := []struct {
		name string
		w    Window
		want bool
	}{
		{"empty scope is a wildcard", Window{}, true},
		{"matching target", Window{Targets: []string{"chan1"}}, true},
		{"other target", Window{Targets: []string{"chan2"}}, false},
		{"target in a list", Window{Targets: []string{"chan2", "chan1"}}, true},
		{"matching check", Window{Checks: []string{"playlist_stalled"}}, true},
		{"other check", Window{Checks: []string{"segment_availability"}}, false},
		{"both match", Window{Targets: []string{"chan1"}, Checks: []string{"playlist_stalled"}}, true},
		{"target matches, check does not", Window{Targets: []string{"chan1"}, Checks: []string{"pdt_stale"}}, false},
	}
	for _, c := range cases {
		if got := c.w.Matches(f); got != c.want {
			t.Errorf("%s: Matches = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestScheduleNamesTheSuppressingWindow(t *testing.T) {
	now := time.Date(2026, 9, 15, 3, 0, 0, 0, time.UTC)
	s := Schedule{
		{Name: "inactive", Loc: time.UTC, Daily: daily(t, "10:00", "11:00")},
		{Name: "encoder restart", Loc: time.UTC, Daily: daily(t, "02:00", "04:00")},
	}
	name, ok := s.Suppressed(Finding{Target: "chan1", Check: "playlist_stalled"}, now)
	if !ok || name != "encoder restart" {
		t.Errorf("Suppressed = (%q, %v), want (\"encoder restart\", true)", name, ok)
	}
}

func TestEmptyScheduleSuppressesNothing(t *testing.T) {
	if _, ok := (Schedule{}).Suppressed(Finding{Target: "c"}, time.Now()); ok {
		t.Error("an empty schedule must never suppress")
	}
}

// --- parsing ---

func TestParseClock(t *testing.T) {
	cases := map[string]int{"00:00": 0, "02:00": 120, "13:45": 825, "23:59": 1439, " 04:30 ": 270}
	for in, want := range cases {
		got, err := ParseClock(in)
		if err != nil {
			t.Errorf("ParseClock(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseClock(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"24:00", "2:00pm", "abc", "", "25:61"} {
		if _, err := ParseClock(bad); err == nil {
			t.Errorf("ParseClock(%q) should have errored", bad)
		}
	}
}

func TestParseWeekday(t *testing.T) {
	cases := map[string]time.Weekday{
		"Mon": time.Monday, "monday": time.Monday, "SUN": time.Sunday,
		"Sat": time.Saturday, "thursday": time.Thursday, " fri ": time.Friday,
	}
	for in, want := range cases {
		got, err := ParseWeekday(in)
		if err != nil {
			t.Errorf("ParseWeekday(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseWeekday(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseWeekday("Caturday"); err == nil {
		t.Error("ParseWeekday should reject nonsense")
	}
}
