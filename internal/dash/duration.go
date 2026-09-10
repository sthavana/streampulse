package dash

import (
	"encoding/xml"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Duration is an xs:duration attribute (ISO 8601, e.g. "PT4.5S", "P1DT2H30M").
//
// The zero value means the attribute was absent, which DASH distinguishes from
// a present value of zero: an absent @minimumUpdatePeriod means the manifest is
// never refetched, while PT0S means refetch as often as you can.
type Duration struct {
	Value time.Duration
	Set   bool
}

// Or returns the duration, or def if the attribute was absent.
func (d Duration) Or(def time.Duration) time.Duration {
	if !d.Set {
		return def
	}
	return d.Value
}

// UnmarshalXMLAttr parses an xs:duration. A value that does not parse leaves
// the attribute unset rather than failing the whole manifest: one bad
// @suggestedPresentationDelay should not cost us every other check.
func (d *Duration) UnmarshalXMLAttr(attr xml.Attr) error {
	v, err := ParseDuration(attr.Value)
	if err != nil {
		return nil
	}
	d.Value, d.Set = v, true
	return nil
}

// DateTime is an xs:dateTime attribute. The zero value means absent.
type DateTime struct {
	time.Time
}

// UnmarshalXMLAttr parses an xs:dateTime, tolerantly: see Duration.
func (t *DateTime) UnmarshalXMLAttr(attr xml.Attr) error {
	v, err := ParseDateTime(attr.Value)
	if err != nil {
		return nil
	}
	t.Time = v
	return nil
}

// Nominal lengths for the year and month designators. ISO 8601 years and
// months have no fixed length, so any duration using them is approximate --
// but no real manifest expresses a segment or buffer length in years, and
// refusing to parse would silently drop the attribute instead.
const (
	nominalYear  = 365 * 24 * time.Hour
	nominalMonth = 30 * 24 * time.Hour
)

// ParseDuration parses an ISO 8601 duration as used by xs:duration.
func ParseDuration(s string) (time.Duration, error) {
	raw := s
	s = strings.TrimSpace(s)
	neg := false
	switch {
	case strings.HasPrefix(s, "-"):
		neg, s = true, s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	if !strings.HasPrefix(s, "P") {
		return 0, fmt.Errorf("duration %q does not start with P", raw)
	}
	s = s[1:]

	var (
		total  time.Duration
		num    strings.Builder
		inTime bool
		fields int
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 'T' {
			if inTime || num.Len() > 0 {
				return 0, fmt.Errorf("misplaced T in duration %q", raw)
			}
			inTime = true
			continue
		}
		if c >= '0' && c <= '9' || c == '.' {
			num.WriteByte(c)
			continue
		}
		if c == ',' { // ISO 8601 permits a comma as the decimal separator
			num.WriteByte('.')
			continue
		}
		if num.Len() == 0 {
			return 0, fmt.Errorf("designator %q with no quantity in duration %q", string(c), raw)
		}
		f, err := strconv.ParseFloat(num.String(), 64)
		if err != nil {
			return 0, fmt.Errorf("bad quantity %q in duration %q", num.String(), raw)
		}
		num.Reset()

		var unit time.Duration
		switch c {
		case 'Y':
			unit = nominalYear
		case 'M':
			// M is minutes after the T and months before it.
			if inTime {
				unit = time.Minute
			} else {
				unit = nominalMonth
			}
		case 'W':
			unit = 7 * 24 * time.Hour
		case 'D':
			unit = 24 * time.Hour
		case 'H', 'S':
			if !inTime {
				return 0, fmt.Errorf("designator %q before T in duration %q", string(c), raw)
			}
			if c == 'H' {
				unit = time.Hour
			} else {
				unit = time.Second
			}
		default:
			return 0, fmt.Errorf("unknown designator %q in duration %q", string(c), raw)
		}
		total += time.Duration(math.Round(f * float64(unit)))
		fields++
	}
	if num.Len() > 0 {
		return 0, fmt.Errorf("quantity %q with no designator in duration %q", num.String(), raw)
	}
	if fields == 0 {
		return 0, fmt.Errorf("duration %q has no components", raw)
	}
	if neg {
		total = -total
	}
	return total, nil
}

// dateTimeLayouts covers xs:dateTime as manifests actually write it. A value
// with no zone offset is read as UTC: DASH wall-clock times are UTC by
// convention, and reading them in the prober's local zone would put the live
// edge hours out.
var dateTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02",
}

// ParseDateTime parses an xs:dateTime.
func ParseDateTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	var (
		t   time.Time
		err error
	)
	for _, l := range dateTimeLayouts {
		if t, err = time.Parse(l, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparsable dateTime %q", s)
}
