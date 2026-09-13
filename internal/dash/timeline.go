package dash

import "time"

// Discontinuity is a break in a SegmentTimeline: a point where one run of
// segments does not continue where the previous one stopped.
//
// A timeline is a contract about media time. Each S element states a start
// @t, a duration @d and a repeat count @r, and the next run is expected to
// begin exactly where the last one ended. When it does not, the presentation
// has either a hole in it or two segments claiming the same instant, and a
// player reaching that point stalls, skips, or re-buffers depending on how
// forgiving it is. Nothing else in a probe sees this: every segment fetches,
// the manifest parses, the edge advances.
type Discontinuity struct {
	// At is where the break falls, as a presentation time relative to the
	// start of the period.
	At time.Duration
	// Delta is the size of the break: positive for a gap, negative for an
	// overlap.
	Delta time.Duration
	// Number is the segment number the break sits in front of.
	Number int64
}

// Gap reports a hole, as opposed to two runs overlapping.
func (d Discontinuity) Gap() bool { return d.Delta > 0 }

// minBreak is the smallest discontinuity worth reporting.
//
// Timeline arithmetic is exact -- @t and @d are integers in the same timescale
// -- so a genuine break is never sub-millisecond. Anything smaller than this
// is a packager rounding a duration somewhere, and no player has a frame short
// enough to miss it.
const minBreak = 10 * time.Millisecond

// TimelineBreaks walks the representation's SegmentTimeline and returns every
// discontinuity in it, oldest first.
//
// The walk mirrors the one in timelineSegments, deliberately: if the two ever
// disagree about where a run begins, the segment URLs this tool fetches are
// not the ones it is reasoning about. It stops short of expanding the runs,
// because a break lives between them.
//
// An open-ended run (@r="-1") ends wherever the next run says it does -- that
// is what the spec means by "until the next S element" -- so a boundary after
// one cannot be a gap and is not treated as one. Reporting those would fire on
// every correct live manifest that uses the form.
func (r *Representation) TimelineBreaks() []Discontinuity {
	t := r.SegmentTemplate
	if t == nil || t.Timeline == nil || len(t.Timeline.S) == 0 {
		return nil
	}
	ts := t.timescale()
	pto := int64(t.presentationTimeOffset())
	number := t.startNumber()

	var (
		out       []Discontinuity
		tick      = pto
		openEnded bool
	)
	for i, s := range t.Timeline.S {
		if s.T != nil {
			at := int64(*s.T)
			if i > 0 && !openEnded {
				if delta := ticksToDuration(at-tick, ts); abs(delta) >= minBreak {
					out = append(out, Discontinuity{
						At:     ticksToDuration(tick-pto, ts),
						Delta:  delta,
						Number: number,
					})
				}
			}
			tick = at
			// Wherever the open-ended run ended, this @t is where the
			// timeline is now, and the runs after it can be compared again.
			openEnded = false
		}
		if s.N != nil {
			number = int64(*s.N)
		}
		if s.D == 0 {
			// A zero-duration run contributes nothing and cannot be stepped
			// through; the expander skips it, so this does too.
			continue
		}
		if s.R < 0 {
			openEnded = true
			continue
		}
		count := int64(s.R) + 1
		tick += int64(s.D) * count
		number += count
	}
	return out
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
