package alert

import (
	"fmt"
	"strings"
	"time"
)

// Daily is a window that recurs every day (optionally only on certain
// weekdays), expressed in minutes since midnight in the window's location.
type Daily struct {
	StartMin int
	EndMin   int
	Days     []time.Weekday // empty means every day
}

// Window is one maintenance period during which alerting is suppressed.
// It is either one-off (Start/End set) or recurring (Daily set).
type Window struct {
	Name    string
	Targets []string // empty matches every target
	Checks  []string // empty matches every check
	Loc     *time.Location
	Start   time.Time // one-off
	End     time.Time
	Daily   *Daily
}

// Matches reports whether the window covers this finding's scope.
func (w Window) Matches(f Finding) bool {
	return contains(w.Targets, f.Target) && contains(w.Checks, f.Check)
}

func contains(set []string, v string) bool {
	if len(set) == 0 {
		return true // an empty set is a wildcard
	}
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// Active reports whether the window is open at now.
func (w Window) Active(now time.Time) bool {
	loc := w.Loc
	if loc == nil {
		loc = time.UTC
	}
	if w.Daily == nil {
		// One-off: half-open so two adjacent windows do not overlap.
		return !now.Before(w.Start) && now.Before(w.End)
	}

	local := now.In(loc)
	min := local.Hour()*60 + local.Minute()
	d := w.Daily

	if d.StartMin < d.EndMin {
		return min >= d.StartMin && min < d.EndMin && dayAllowed(d.Days, local.Weekday())
	}
	// Crosses midnight (e.g. 22:00-02:00). The weekday that gates the window is
	// the day it *opened*, so the small hours belong to the previous day.
	if min >= d.StartMin {
		return dayAllowed(d.Days, local.Weekday())
	}
	if min < d.EndMin {
		return dayAllowed(d.Days, local.AddDate(0, 0, -1).Weekday())
	}
	return false
}

func dayAllowed(days []time.Weekday, wd time.Weekday) bool {
	if len(days) == 0 {
		return true
	}
	for _, d := range days {
		if d == wd {
			return true
		}
	}
	return false
}

// Schedule is the set of configured maintenance windows.
type Schedule []Window

// Suppressed reports whether any window covers this finding right now, and
// names the first one that does.
func (s Schedule) Suppressed(f Finding, now time.Time) (string, bool) {
	for _, w := range s {
		if w.Matches(f) && w.Active(now) {
			return w.Name, true
		}
	}
	return "", false
}

// --- parsing helpers, shared with package config ---

// ParseClock reads a "15:04" wall-clock time as minutes since midnight.
func ParseClock(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid time of day %q, want HH:MM", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday,
	"mon": time.Monday, "monday": time.Monday,
	"tue": time.Tuesday, "tuesday": time.Tuesday, "tues": time.Tuesday,
	"wed": time.Wednesday, "wednesday": time.Wednesday,
	"thu": time.Thursday, "thursday": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday,
	"fri": time.Friday, "friday": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday,
}

// ParseWeekday accepts "Mon" or "Monday", case-insensitively.
func ParseWeekday(s string) (time.Weekday, error) {
	if wd, ok := weekdays[strings.ToLower(strings.TrimSpace(s))]; ok {
		return wd, nil
	}
	return 0, fmt.Errorf("unknown weekday %q", s)
}
