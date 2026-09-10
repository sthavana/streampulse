package dash

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// maxSegments caps how many segments one representation will enumerate.
//
// A live stream that has been up for a month at 4s segments is 650k segments;
// a SegmentTimeline with @r="-1" and an unusable @t is unbounded. Neither is
// something a prober needs in full -- it samples the recent end of the window
// -- so enumeration stops rather than growing a slice until the process dies.
const maxSegments = 20000

// Segment is one addressable media segment.
type Segment struct {
	// URI is fully resolved against the representation's base URL.
	URI string
	// Number is the value $Number$ took, or the position in a SegmentList.
	Number int64
	// Time is the value $Time$ took: the segment's media start time in
	// timescale units. Zero for purely number-addressed segments.
	Time uint64
	// Start is the segment's presentation start, relative to the start of its
	// period.
	Start time.Duration
	// Duration is the segment's length as declared by the manifest.
	Duration time.Duration
	// Available is the wall-clock time from which a player may fetch this
	// segment. Zero for a static presentation, where everything listed is
	// available already.
	Available time.Time
}

// End is the segment's presentation end, relative to the start of its period.
func (s Segment) End() time.Duration { return s.Start + s.Duration }

// Addressing names how a representation's segments are located.
type Addressing string

const (
	// AddressingTimeline is SegmentTemplate with an explicit SegmentTimeline.
	AddressingTimeline Addressing = "template-timeline"
	// AddressingNumber is SegmentTemplate with @duration: segment n is at
	// n*duration, and the live edge is a function of the clock.
	AddressingNumber Addressing = "template-number"
	// AddressingList is an explicit SegmentList.
	AddressingList Addressing = "list"
	// AddressingBase is a single file, indexed internally by a sidx box.
	AddressingBase Addressing = "base"
	// AddressingNone is a representation we cannot locate segments for.
	AddressingNone Addressing = "none"
)

// Addressing reports how this representation's segments are located.
func (r *Representation) Addressing() Addressing {
	switch t := r.SegmentTemplate; {
	case t != nil && t.Media != "" && t.Timeline != nil:
		return AddressingTimeline
	case t != nil && t.Media != "" && t.Duration != nil:
		return AddressingNumber
	case r.SegmentList != nil && len(r.SegmentList.SegmentURLs) > 0:
		return AddressingList
	case r.SegmentBase != nil || r.base != "":
		return AddressingBase
	default:
		return AddressingNone
	}
}

// InitURI is the initialisation segment a player fetches before any media
// segment, resolved. It is "" when the representation declares none.
//
// A SegmentBase representation has its initialisation inside its own file as a
// byte range, so the representation's base URL is returned: that is the
// resource to fetch either way.
func (r *Representation) InitURI() string {
	if t := r.SegmentTemplate; t != nil && t.Initialization != "" {
		return resolveRef(r.base, r.expand(t.Initialization, 0, 0))
	}
	if l := r.SegmentList; l != nil && l.Initialization != nil && l.Initialization.SourceURL != "" {
		return resolveRef(r.base, l.Initialization.SourceURL)
	}
	if b := r.SegmentBase; b != nil {
		if b.Initialization != nil && b.Initialization.SourceURL != "" {
			return resolveRef(r.base, b.Initialization.SourceURL)
		}
		return r.base
	}
	return ""
}

// SegmentsAt returns the segments a player could fetch at wall-clock time now,
// oldest first.
//
// For a static presentation now is ignored and the whole period is returned.
// For a dynamic one the result is the availability window: segments that have
// finished being produced (their end is at or before the live edge, allowing
// for @availabilityTimeOffset) and have not yet fallen out of
// @timeShiftBufferDepth. That window is the honest answer to "what would a
// player see right now", which is what a segment-availability check needs --
// asking a CDN for a segment the packager has not published yet is a 404 that
// means nothing.
func (r *Representation) SegmentsAt(now time.Time) []Segment {
	switch r.Addressing() {
	case AddressingTimeline:
		return r.timelineSegments(now)
	case AddressingNumber:
		return r.numberSegments(now)
	case AddressingList:
		return r.listSegments()
	case AddressingBase:
		return r.baseSegment()
	default:
		return nil
	}
}

// window is the span of period-relative presentation time whose segments are
// fetchable at some instant.
type window struct {
	// earliest is the oldest segment *end* still inside the DVR window.
	earliest time.Duration
	// latest is the newest segment end that has been published.
	latest time.Duration
	// bounded reports whether latest is a real limit. It is false for a static
	// presentation of unknown length, where everything the manifest lists is
	// available and nothing bounds an open-ended run.
	bounded bool
	// live reports whether latest is the live edge rather than the end of a
	// period. The two truncate differently: a period ends with a short final
	// segment, which belongs in the window, whereas a segment straddling the
	// live edge has not been published yet and does not.
	live bool
}

// availabilityAt computes the window for this representation at now.
func (r *Representation) availabilityAt(now time.Time) window {
	p := r.set.period
	m := p.mpd
	w := window{}

	// A period that has already ended never extends past its own end, even in
	// a live manifest -- this is the completed period before an ad break.
	if end, ok := p.End(); ok {
		w.latest, w.bounded = end-p.Start, true
	}

	// Static, or dynamic with no availabilityStartTime to anchor the clock to:
	// there is no live edge to compute, so take the manifest at its word.
	// (A dynamic MPD without @availabilityStartTime is malformed; treating it
	// as fully available is the tolerant reading, and the check layer is the
	// right place to say so.)
	if !m.Dynamic() || m.AvailabilityStartTime.IsZero() {
		return w
	}

	ato := time.Duration(r.SegmentTemplate.availabilityTimeOffset() * float64(time.Second))
	edge := now.UTC().Sub(m.AvailabilityStartTime.Time) - p.Start + ato
	if !w.bounded || edge < w.latest {
		w.latest, w.bounded, w.live = edge, true, true
	}
	if d := m.TimeShiftBufferDepth; d.Set && d.Value > 0 {
		w.earliest = w.latest - d.Value
	}
	return w
}

// timelineSegments walks a SegmentTimeline.
func (r *Representation) timelineSegments(now time.Time) []Segment {
	t := r.SegmentTemplate
	ts := t.timescale()
	pto := int64(t.presentationTimeOffset())
	number := t.startNumber()
	w := r.availabilityAt(now)

	// The tick at which an open-ended run (@r="-1") stops: the live edge, or
	// the end of a period that declares its length.
	var limitTick int64
	if w.bounded {
		limitTick = pto + durationToTicks(w.latest, ts)
	}

	var (
		out  []Segment
		tick = pto
	)
	for _, s := range t.Timeline.S {
		if s.T != nil {
			tick = int64(*s.T)
		}
		if s.N != nil {
			number = int64(*s.N)
		}
		if s.D == 0 {
			// A zero-duration run cannot be stepped through; walking it would
			// spin forever. Skip it and keep the rest of the timeline.
			continue
		}
		d := int64(s.D)

		repeats := s.R
		if repeats < 0 {
			// Repeat to the end of the period, or to the live edge.
			repeats = 0
			if w.bounded && limitTick > tick {
				if n := (limitTick - tick) / d; n > 0 {
					repeats = int(min64(n, maxSegments))
				}
			}
		}
		for i := 0; i <= repeats; i++ {
			seg := Segment{
				Number:   number,
				Time:     uint64(maxInt64(tick, 0)),
				Start:    ticksToDuration(tick-pto, ts),
				Duration: ticksToDuration(d, ts),
			}
			seg.URI = resolveRef(r.base, r.expand(t.Media, seg.Number, seg.Time))
			r.stamp(&seg)
			if inWindow(seg, w) {
				out = append(out, seg)
			}
			tick += d
			number++
			if len(out) >= maxSegments {
				return out
			}
		}
	}
	return out
}

// numberSegments handles SegmentTemplate with @duration, where segment i
// simply starts at i*duration.
//
// The segment range is computed arithmetically rather than by counting up from
// the start of the presentation: a stream that has been live for a week has
// hundreds of thousands of segments behind it and only the last few minutes
// are fetchable.
func (r *Representation) numberSegments(now time.Time) []Segment {
	t := r.SegmentTemplate
	ts := t.timescale()
	d := ticksToDuration(int64(*t.Duration), ts)
	if d <= 0 {
		return nil
	}
	w := r.availabilityAt(now)

	var last int64
	switch {
	case w.live:
		// Segment i ends at (i+1)*d, and must have ended by the live edge to
		// have been published at all.
		last = int64(w.latest/d) - 1
	case w.bounded:
		// A closed period: its final segment may be short, so round up.
		last = int64(math.Ceil(float64(w.latest)/float64(d))) - 1
	default:
		// A presentation with neither a live edge nor a declared length gives
		// us nothing to count to.
		return nil
	}
	first := int64(0)
	if w.earliest > 0 {
		first = int64(math.Ceil(float64(w.earliest)/float64(d))) - 1
	}
	if first < 0 {
		first = 0
	}
	if last < first {
		return nil
	}
	if last-first+1 > maxSegments {
		// Keep the newest: on a live stream that is the interesting end.
		first = last - maxSegments + 1
	}

	start := t.startNumber()
	pto := t.presentationTimeOffset()
	out := make([]Segment, 0, last-first+1)
	for i := first; i <= last; i++ {
		seg := Segment{
			Number:   start + i,
			Time:     pto + uint64(i)*(*t.Duration),
			Start:    time.Duration(i) * d,
			Duration: d,
		}
		seg.URI = resolveRef(r.base, r.expand(t.Media, seg.Number, seg.Time))
		r.stamp(&seg)
		out = append(out, seg)
	}
	return out
}

// listSegments enumerates an explicit SegmentList.
func (r *Representation) listSegments() []Segment {
	l := r.SegmentList
	ts := uint64(1)
	if l.Timescale != nil && *l.Timescale > 0 {
		ts = *l.Timescale
	}
	var d time.Duration
	if l.Duration != nil {
		d = ticksToDuration(int64(*l.Duration), ts)
	}
	number := int64(1)
	if l.StartNumber != nil {
		number = *l.StartNumber
	}

	out := make([]Segment, 0, len(l.SegmentURLs))
	for i, su := range l.SegmentURLs {
		if len(out) >= maxSegments {
			break
		}
		seg := Segment{
			URI:      resolveRef(r.base, su.Media),
			Number:   number + int64(i),
			Start:    time.Duration(i) * d,
			Duration: d,
		}
		r.stamp(&seg)
		out = append(out, seg)
	}
	return out
}

// baseSegment covers the on-demand shape: the representation is one file,
// indexed internally. Without reading the sidx box we cannot enumerate the
// segments inside it, but the file itself is fetchable and is what a
// reachability check should ask for.
func (r *Representation) baseSegment() []Segment {
	if r.base == "" {
		return nil
	}
	return []Segment{{
		URI:      r.base,
		Duration: r.set.period.Duration,
	}}
}

// stamp fills in when a segment becomes available on a dynamic presentation.
func (r *Representation) stamp(s *Segment) {
	m := r.set.period.mpd
	if !m.Dynamic() || m.AvailabilityStartTime.IsZero() {
		return
	}
	ato := time.Duration(r.SegmentTemplate.availabilityTimeOffset() * float64(time.Second))
	s.Available = m.AvailabilityStartTime.Time.
		Add(r.set.period.Start).
		Add(s.End()).
		Add(-ato)
}

// inWindow reports whether a segment has been published and has not yet aged
// out of the DVR window.
func inWindow(s Segment, w window) bool {
	switch {
	case w.live && s.End() > w.latest:
		// Straddles the live edge: not published yet.
		return false
	case w.bounded && !w.live && s.Start >= w.latest:
		// Starts after the period ends. A segment that merely overruns the end
		// is kept: that is the normal short final segment.
		return false
	}
	return s.End() >= w.earliest
}

// --- template expansion ---

// expand substitutes the identifiers of a SegmentTemplate @media, @index or
// @initialization pattern (ISO/IEC 23009-1 5.3.9.4.4).
//
// An identifier we do not know is left in place rather than blanked: a URL
// with a visible "$Foo$" in it makes the manifest's mistake obvious in a
// finding, where a silently mangled URL would just look like a 404.
func (r *Representation) expand(tmpl string, number int64, t uint64) string {
	if !strings.Contains(tmpl, "$") {
		return tmpl
	}
	var b strings.Builder
	for i := 0; i < len(tmpl); {
		if tmpl[i] != '$' {
			b.WriteByte(tmpl[i])
			i++
			continue
		}
		end := strings.IndexByte(tmpl[i+1:], '$')
		if end < 0 {
			// Unterminated: emit the rest verbatim.
			b.WriteString(tmpl[i:])
			break
		}
		token := tmpl[i+1 : i+1+end]
		i += end + 2

		if token == "" { // "$$" is an escaped dollar
			b.WriteByte('$')
			continue
		}
		name, format := token, ""
		if p := strings.IndexByte(token, '%'); p >= 0 {
			name, format = token[:p], token[p:]
		}
		switch name {
		case "RepresentationID":
			// Defined as a string, so it takes no format tag.
			b.WriteString(r.ID)
		case "Number":
			b.WriteString(formatIdentifier(format, uint64(maxInt64(number, 0))))
		case "Time":
			b.WriteString(formatIdentifier(format, t))
		case "Bandwidth":
			b.WriteString(formatIdentifier(format, uint64(maxInt64(int64(r.Bandwidth), 0))))
		default:
			b.WriteString("$" + token + "$")
		}
	}
	return b.String()
}

// formatIdentifier applies the optional printf tag on an identifier, which the
// spec restricts to zero-padded decimal ("$Number%05d$"). Anything else is
// ignored rather than handed to fmt, so a malformed tag cannot inject "%!d" or
// a padding of thousands of characters into a URL.
func formatIdentifier(format string, v uint64) string {
	if !validFormatTag(format) {
		return strconv.FormatUint(v, 10)
	}
	return fmt.Sprintf(format, v)
}

func validFormatTag(f string) bool {
	if len(f) < 2 || f[0] != '%' || f[len(f)-1] != 'd' {
		return false
	}
	width := f[1 : len(f)-1]
	if strings.HasPrefix(width, "0") {
		width = width[1:]
	}
	if width == "" {
		return true
	}
	if len(width) > 2 { // no legitimate URL pads to 100 digits
		return false
	}
	for i := 0; i < len(width); i++ {
		if width[i] < '0' || width[i] > '9' {
			return false
		}
	}
	return true
}

// --- small helpers ---

func (t *SegmentTemplate) timescale() uint64 {
	if t != nil && t.Timescale != nil && *t.Timescale > 0 {
		return *t.Timescale
	}
	return 1
}

func (t *SegmentTemplate) startNumber() int64 {
	if t != nil && t.StartNumber != nil {
		return *t.StartNumber
	}
	return 1
}

func (t *SegmentTemplate) presentationTimeOffset() uint64 {
	if t != nil && t.PresentationTimeOffset != nil {
		return *t.PresentationTimeOffset
	}
	return 0
}

func (t *SegmentTemplate) availabilityTimeOffset() float64 {
	if t == nil {
		return 0
	}
	return t.AvailabilityTimeOffset
}

func ticksToDuration(ticks int64, timescale uint64) time.Duration {
	if timescale == 0 {
		timescale = 1
	}
	return time.Duration(float64(ticks) / float64(timescale) * float64(time.Second))
}

func durationToTicks(d time.Duration, timescale uint64) int64 {
	return int64(d.Seconds() * float64(timescale))
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
