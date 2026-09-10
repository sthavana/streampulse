package dash

import (
	"encoding/xml"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"PT4S":        4 * time.Second,
		"PT0S":        0,
		"PT1M30S":     90 * time.Second,
		"PT1H2M3.5S":  time.Hour + 2*time.Minute + 3500*time.Millisecond,
		"P1DT2H":      26 * time.Hour,
		"PT0.5S":      500 * time.Millisecond,
		"PT2,5S":      2500 * time.Millisecond, // comma decimal separator
		"P1W":         7 * 24 * time.Hour,
		"-PT10S":      -10 * time.Second,
		"PT6M":        6 * time.Minute,  // M after T is minutes
		"P6M":         6 * nominalMonth, // M before T is months
		"P1Y":         nominalYear,
		"PT23H59M59S": 23*time.Hour + 59*time.Minute + 59*time.Second,
		"PT9.999999S": 9999999 * time.Microsecond,
	}
	for in, want := range cases {
		got, err := ParseDuration(in)
		if err != nil {
			t.Errorf("ParseDuration(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseDuration(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseDurationRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"",
		"4S",     // no P
		"P",      // no components
		"PT",     // no components
		"PTS",    // designator with no quantity
		"P1H",    // H belongs after the T
		"P1S",    // so does S
		"PT1X",   // unknown designator
		"PT1M2",  // trailing quantity
		"PTTT1S", // misplaced T
		"garbage",
	} {
		if d, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) = %v, want an error", in, d)
		}
	}
}

// An attribute that does not parse must not cost us the rest of the manifest.
func TestDurationAttrIsTolerant(t *testing.T) {
	var d Duration
	if err := d.UnmarshalXMLAttr(xml.Attr{Value: "PT-not-a-duration"}); err != nil {
		t.Fatalf("a bad duration should not error the decode: %v", err)
	}
	if d.Set {
		t.Errorf("a bad duration should leave the attribute unset, got %v", d.Value)
	}
}

// PT0S and an absent attribute mean different things in DASH, so the parsed
// value has to tell them apart.
func TestDurationDistinguishesZeroFromAbsent(t *testing.T) {
	var absent Duration
	if got := absent.Or(time.Minute); got != time.Minute {
		t.Errorf("absent.Or(1m) = %v, want 1m", got)
	}
	var zero Duration
	if err := zero.UnmarshalXMLAttr(xml.Attr{Value: "PT0S"}); err != nil {
		t.Fatal(err)
	}
	if !zero.Set {
		t.Fatal("PT0S should be Set")
	}
	if got := zero.Or(time.Minute); got != 0 {
		t.Errorf("PT0S.Or(1m) = %v, want 0", got)
	}
}

func TestParseDateTime(t *testing.T) {
	want := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for _, in := range []string{
		"2026-09-09T12:00:00Z",
		"2026-09-09T12:00:00.000Z",
		"2026-09-09T14:00:00+02:00",
		"2026-09-09T12:00:00", // no zone: read as UTC
	} {
		got, err := ParseDateTime(in)
		if err != nil {
			t.Errorf("ParseDateTime(%q) errored: %v", in, err)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("ParseDateTime(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseDateTime("last tuesday"); err == nil {
		t.Error("expected an error on an unparsable dateTime")
	}
}
