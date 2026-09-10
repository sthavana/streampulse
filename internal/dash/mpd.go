// Package dash provides a small, dependency-free parser for MPEG-DASH media
// presentation descriptions (ISO/IEC 23009-1). Like the hls package it is
// deliberately tolerant: packagers emit MPDs that bend the schema, and a
// monitoring tool should keep going and flag problems rather than fail to
// parse.
//
// Parsing an MPD is more than reading XML. Almost nothing a prober needs is
// stated outright: segment URLs live in templates, period start times are
// often implied by the periods around them, BaseURL and SegmentTemplate are
// inherited down four levels, and on a live stream which segments exist at all
// is a function of wall-clock time. Parse therefore resolves all of that up
// front, so callers work with representations that know their own base URL,
// their own effective template, and how to enumerate the segments a player
// could fetch right now (see Representation.SegmentsAt).
package dash

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

// MPD is a parsed media presentation description.
type MPD struct {
	XMLName                    xml.Name             `xml:"MPD"`
	ID                         string               `xml:"id,attr"`
	Type                       string               `xml:"type,attr"`
	Profiles                   string               `xml:"profiles,attr"`
	AvailabilityStartTime      DateTime             `xml:"availabilityStartTime,attr"`
	AvailabilityEndTime        DateTime             `xml:"availabilityEndTime,attr"`
	PublishTime                DateTime             `xml:"publishTime,attr"`
	MinimumUpdatePeriod        Duration             `xml:"minimumUpdatePeriod,attr"`
	MinBufferTime              Duration             `xml:"minBufferTime,attr"`
	TimeShiftBufferDepth       Duration             `xml:"timeShiftBufferDepth,attr"`
	SuggestedPresentationDelay Duration             `xml:"suggestedPresentationDelay,attr"`
	MediaPresentationDuration  Duration             `xml:"mediaPresentationDuration,attr"`
	MaxSegmentDuration         Duration             `xml:"maxSegmentDuration,attr"`
	BaseURLs                   []string             `xml:"BaseURL"`
	Locations                  []string             `xml:"Location"`
	UTCTimings                 []UTCTiming          `xml:"UTCTiming"`
	ServiceDescriptions        []ServiceDescription `xml:"ServiceDescription"`
	Periods                    []*Period            `xml:"Period"`

	// base is the effective BaseURL for the document: the URL the manifest was
	// fetched from, with any MPD-level BaseURL resolved against it.
	base string
}

// UTCTiming declares how a client should learn the wall-clock time the
// publisher is using. Worth keeping: a live edge that looks frozen is
// sometimes just clock skew between the packager and the prober.
type UTCTiming struct {
	SchemeIDURI string `xml:"schemeIdUri,attr"`
	Value       string `xml:"value,attr"`
}

// ServiceDescription states the latency the operator is aiming for. It is how
// a low-latency stream declares itself: without it there is nothing to say
// whether a three-second edge delay is the design or a fault.
type ServiceDescription struct {
	ID       int       `xml:"id,attr"`
	Latency  *Latency  `xml:"Latency"`
	Playback *Playback `xml:"PlaybackRate"`
}

// Latency is the target and bounds, in milliseconds, of the delay between
// capture and playback (DASH-IF low-latency guidelines).
type Latency struct {
	Target      int `xml:"target,attr"`
	Min         int `xml:"min,attr"`
	Max         int `xml:"max,attr"`
	ReferenceID int `xml:"referenceId,attr"`
}

// Playback is the rate range a player may use to catch up to the live edge.
type Playback struct {
	Min float64 `xml:"min,attr"`
	Max float64 `xml:"max,attr"`
}

// Period is one period of the presentation. A live stream with ad insertion
// opens and closes periods continuously.
type Period struct {
	ID   string `xml:"id,attr"`
	Href string `xml:"href,attr"`
	// RawStart and RawDuration are the attributes as written. Both are
	// routinely absent; use the resolved Start and Duration fields instead.
	RawStart        Duration         `xml:"start,attr"`
	RawDuration     Duration         `xml:"duration,attr"`
	BaseURLs        []string         `xml:"BaseURL"`
	SegmentTemplate *SegmentTemplate `xml:"SegmentTemplate"`
	SegmentList     *SegmentList     `xml:"SegmentList"`
	SegmentBase     *SegmentBase     `xml:"SegmentBase"`
	AdaptationSets  []*AdaptationSet `xml:"AdaptationSet"`

	// Start is the presentation time at which this period begins, resolved
	// from @start or inferred from the neighbouring periods.
	Start time.Duration
	// Duration is the period's length, resolved from @duration, from the next
	// period's start, or from @mediaPresentationDuration. It is zero when the
	// length is genuinely unknown, which is the normal case for the last
	// period of a live stream.
	Duration time.Duration

	mpd  *MPD
	base string
}

// AdaptationSet groups the representations that are alternatives for one
// another: the video ladder, one audio language, one subtitle track.
type AdaptationSet struct {
	ID    string `xml:"id,attr"`
	Group int    `xml:"group,attr"`
	// ContentType names what the set carries -- video, audio, text. It is
	// optional in the schema; Parse fills it in from the mimeType when the
	// packager left it out.
	ContentType        string              `xml:"contentType,attr"`
	MimeType           string              `xml:"mimeType,attr"`
	Lang               string              `xml:"lang,attr"`
	Codecs             string              `xml:"codecs,attr"`
	Width              int                 `xml:"width,attr"`
	Height             int                 `xml:"height,attr"`
	FrameRate          string              `xml:"frameRate,attr"`
	SegmentAlignment   bool                `xml:"segmentAlignment,attr"`
	Roles              []Descriptor        `xml:"Role"`
	ContentProtections []ContentProtection `xml:"ContentProtection"`
	BaseURLs           []string            `xml:"BaseURL"`
	SegmentTemplate    *SegmentTemplate    `xml:"SegmentTemplate"`
	SegmentList        *SegmentList        `xml:"SegmentList"`
	SegmentBase        *SegmentBase        `xml:"SegmentBase"`
	Representations    []*Representation   `xml:"Representation"`

	period *Period
	base   string
}

// Representation is one encoding of an adaptation set -- one rung of the
// ladder. After Parse its BaseURL, SegmentTemplate, SegmentList, SegmentBase
// and ContentProtections are the *effective* ones, with everything inherited
// from the adaptation set, period and MPD already folded in.
type Representation struct {
	ID                 string              `xml:"id,attr"`
	Bandwidth          int                 `xml:"bandwidth,attr"`
	MimeType           string              `xml:"mimeType,attr"`
	Codecs             string              `xml:"codecs,attr"`
	Width              int                 `xml:"width,attr"`
	Height             int                 `xml:"height,attr"`
	FrameRate          string              `xml:"frameRate,attr"`
	AudioSamplingRate  string              `xml:"audioSamplingRate,attr"`
	ContentProtections []ContentProtection `xml:"ContentProtection"`
	BaseURLs           []string            `xml:"BaseURL"`
	SegmentTemplate    *SegmentTemplate    `xml:"SegmentTemplate"`
	SegmentList        *SegmentList        `xml:"SegmentList"`
	SegmentBase        *SegmentBase        `xml:"SegmentBase"`

	set  *AdaptationSet
	base string
}

// Descriptor is the schemeIdUri/value pair DASH uses for Role, Accessibility
// and friends.
type Descriptor struct {
	SchemeIDURI string `xml:"schemeIdUri,attr"`
	Value       string `xml:"value,attr"`
}

// SegmentTemplate builds segment URLs from a pattern rather than listing them.
// The pointer-typed attributes distinguish absent from zero, which matters
// because a template inherits attribute by attribute from the level above it.
type SegmentTemplate struct {
	Media                  string  `xml:"media,attr"`
	Initialization         string  `xml:"initialization,attr"`
	Index                  string  `xml:"index,attr"`
	Timescale              *uint64 `xml:"timescale,attr"`
	Duration               *uint64 `xml:"duration,attr"`
	StartNumber            *int64  `xml:"startNumber,attr"`
	PresentationTimeOffset *uint64 `xml:"presentationTimeOffset,attr"`
	AvailabilityTimeOffset float64 `xml:"availabilityTimeOffset,attr"`
	// AvailabilityTimeComplete false means a segment is fetchable before it
	// has finished being produced -- the defining property of chunked
	// low-latency delivery. Absent means true.
	AvailabilityTimeComplete *bool            `xml:"availabilityTimeComplete,attr"`
	Timeline                 *SegmentTimeline `xml:"SegmentTimeline"`
}

// SegmentTimeline lists segment durations explicitly, which is how any stream
// with variable segment lengths (most live streams with ad breaks) is
// addressed.
type SegmentTimeline struct {
	S []S `xml:"S"`
}

// S is one run of equal-length segments in a SegmentTimeline: start time @t,
// duration @d, and @r *additional* repeats. @r of -1 means "repeat to the end
// of the period", which on a live stream means to the live edge.
type S struct {
	T *uint64 `xml:"t,attr"`
	D uint64  `xml:"d,attr"`
	R int     `xml:"r,attr"`
	N *uint64 `xml:"n,attr"`
}

// SegmentList addresses segments by listing their URLs.
type SegmentList struct {
	Timescale      *uint64          `xml:"timescale,attr"`
	Duration       *uint64          `xml:"duration,attr"`
	StartNumber    *int64           `xml:"startNumber,attr"`
	Initialization *URL             `xml:"Initialization"`
	SegmentURLs    []SegmentURL     `xml:"SegmentURL"`
	Timeline       *SegmentTimeline `xml:"SegmentTimeline"`
}

// SegmentURL is one entry in a SegmentList.
type SegmentURL struct {
	Media      string `xml:"media,attr"`
	MediaRange string `xml:"mediaRange,attr"`
	Index      string `xml:"index,attr"`
	IndexRange string `xml:"indexRange,attr"`
}

// SegmentBase addresses a representation as a single file with an index, the
// usual shape for on-demand profiles.
type SegmentBase struct {
	Timescale              *uint64 `xml:"timescale,attr"`
	PresentationTimeOffset *uint64 `xml:"presentationTimeOffset,attr"`
	IndexRange             string  `xml:"indexRange,attr"`
	Initialization         *URL    `xml:"Initialization"`
}

// URL is the URLType element used for Initialization and RepresentationIndex.
type URL struct {
	SourceURL string `xml:"sourceURL,attr"`
	Range     string `xml:"range,attr"`
}

// ContentProtection declares that a representation is encrypted, and under
// which DRM system.
//
// The PSSH is exposed as raw bytes rather than interpreted here: parsing and
// naming DRM systems belongs to the check layer, which already does it for
// HLS, and duplicating it would let the two drift apart.
type ContentProtection struct {
	SchemeIDURI string `xml:"schemeIdUri,attr"`
	Value       string `xml:"value,attr"`
	DefaultKID  string `xml:"default_KID,attr"`
	// PSSH is the base64 body of a cenc:pssh child element, if there is one.
	PSSH string `xml:"pssh"`
}

// SystemID returns the DRM system UUID from a "urn:uuid:" schemeIdUri,
// lowercased, or "" for the schemes that are not one -- notably
// urn:mpeg:dash:mp4protection:2011, which announces the encryption scheme
// (cenc/cbcs) rather than a system.
func (c ContentProtection) SystemID() string {
	s := strings.ToLower(strings.TrimSpace(c.SchemeIDURI))
	if !strings.HasPrefix(s, "urn:uuid:") {
		return ""
	}
	return strings.TrimPrefix(s, "urn:uuid:")
}

// PSSHBytes decodes the cenc:pssh payload. The bool reports whether there was
// one to decode and it was valid base64.
func (c ContentProtection) PSSHBytes() ([]byte, bool) {
	s := strings.Join(strings.Fields(c.PSSH), "")
	if s == "" {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	return b, true
}

// Dynamic reports whether this is a live presentation, whose segment set
// changes with wall-clock time. @type defaults to "static".
func (m *MPD) Dynamic() bool { return strings.EqualFold(m.Type, "dynamic") }

// BaseURL is the effective base URL of the document: the URL it was fetched
// from with any MPD-level BaseURL resolved against it.
func (m *MPD) BaseURL() string { return m.base }

// Latency returns the declared latency target and whether one was declared.
// DASH states it in milliseconds.
func (m *MPD) Latency() (target time.Duration, ok bool) {
	for _, sd := range m.ServiceDescriptions {
		if sd.Latency != nil && sd.Latency.Target > 0 {
			return time.Duration(sd.Latency.Target) * time.Millisecond, true
		}
	}
	return 0, false
}

// MaxLatency returns the declared upper bound on latency, if there is one. It
// is the operator's own statement of what counts as too far behind, which
// beats any threshold this tool could invent.
func (m *MPD) MaxLatency() (time.Duration, bool) {
	for _, sd := range m.ServiceDescriptions {
		if sd.Latency != nil && sd.Latency.Max > 0 {
			return time.Duration(sd.Latency.Max) * time.Millisecond, true
		}
	}
	return 0, false
}

// LowLatency reports whether the presentation is built for low latency.
//
// Two things say so independently: a declared latency target, and a template
// that publishes segments before they are complete. Either is enough, because
// packagers are inconsistent about which they emit.
func (m *MPD) LowLatency() bool {
	if _, ok := m.Latency(); ok {
		return true
	}
	for _, r := range m.Representations() {
		if t := r.SegmentTemplate; t != nil && t.chunked() {
			return true
		}
	}
	return false
}

// Representations flattens every representation in the presentation, in
// document order.
func (m *MPD) Representations() []*Representation {
	var out []*Representation
	for _, p := range m.Periods {
		for _, a := range p.AdaptationSets {
			out = append(out, a.Representations...)
		}
	}
	return out
}

// Encrypted reports whether any adaptation set or representation declares
// ContentProtection.
func (m *MPD) Encrypted() bool {
	for _, p := range m.Periods {
		for _, a := range p.AdaptationSets {
			if len(a.ContentProtections) > 0 {
				return true
			}
			for _, r := range a.Representations {
				if len(r.ContentProtections) > 0 {
					return true
				}
			}
		}
	}
	return false
}

// End returns the presentation time at which the period ends, and whether that
// is known. It is not known for the last period of a live stream.
func (p *Period) End() (time.Duration, bool) {
	if p.Duration <= 0 {
		return 0, false
	}
	return p.Start + p.Duration, true
}

// MPD returns the presentation this period belongs to.
func (p *Period) MPD() *MPD { return p.mpd }

// BaseURL is the effective base URL for the period.
func (p *Period) BaseURL() string { return p.base }

// resolveContentType fills in @contentType from a mimeType when the packager
// omitted it, so that consumers -- metric labels above all -- always have a
// word for what the set carries.
func (a *AdaptationSet) resolveContentType() {
	if strings.TrimSpace(a.ContentType) != "" {
		a.ContentType = strings.TrimSpace(a.ContentType)
		return
	}
	mime := a.MimeType
	if mime == "" {
		for _, r := range a.Representations {
			if r.MimeType != "" {
				mime = r.MimeType
				break
			}
		}
	}
	if i := strings.Index(mime, "/"); i > 0 {
		a.ContentType = mime[:i]
	}
}

// Period returns the period this adaptation set belongs to.
func (a *AdaptationSet) Period() *Period { return a.period }

// BaseURL is the effective base URL for the adaptation set.
func (a *AdaptationSet) BaseURL() string { return a.base }

// Set returns the adaptation set this representation belongs to.
func (r *Representation) Set() *AdaptationSet { return r.set }

// Period returns the period this representation belongs to.
func (r *Representation) Period() *Period { return r.set.period }

// BaseURL is the effective base URL for the representation: every BaseURL from
// the document down to this element, resolved in turn against the URL the
// manifest was fetched from.
func (r *Representation) BaseURL() string { return r.base }

// Label renders a representation for use as a metric label and in findings.
//
// The content type and language are included because @id alone is not
// distinctive: it is only required to be unique within a period, and
// packagers routinely number representations 1..n per adaptation set, so
// "audio/en/1" and "audio/de/1" would otherwise collapse into one series and
// one incident. The period is deliberately *not* part of the label: on an
// ad-inserted live stream periods open and close every few minutes, and
// keying on one would churn out a new metric series per ad break.
func (r *Representation) Label() string {
	parts := make([]string, 0, 3)
	if ct := r.set.ContentType; ct != "" {
		parts = append(parts, ct)
	}
	if lang := r.set.Lang; lang != "" {
		parts = append(parts, lang)
	}
	id := r.ID
	if id == "" {
		id = fmt.Sprintf("%dbps", r.Bandwidth)
	}
	return strings.Join(append(parts, id), "/")
}

// LooksLikeMPD reports whether a body is plausibly an MPD, for telling one
// apart from an HLS playlist or from the HTML error page a CDN serves when a
// stream has been retired.
func LooksLikeMPD(raw []byte) bool {
	head := raw
	if len(head) > 4096 {
		head = head[:4096]
	}
	return bytes.Contains(head, []byte("<MPD")) || bytes.Contains(head, []byte(":MPD"))
}

// Parse reads an MPD. base is the URL the manifest was fetched from, used to
// resolve relative BaseURLs and segment URLs; it may be empty, in which case
// resolved URLs stay relative.
func Parse(raw []byte, base string) (*MPD, error) {
	var m MPD
	dec := xml.NewDecoder(bytes.NewReader(raw))
	// Tolerate the two things real manifests do that a strict reader rejects:
	// undeclared entities, and a declared charset we have no table for. DASH
	// is UTF-8 in practice, so the bytes are passed through as they are.
	dec.Strict = false
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse MPD: %w", err)
	}
	if m.XMLName.Local != "MPD" {
		return nil, fmt.Errorf("root element is <%s>, not <MPD>", m.XMLName.Local)
	}
	m.resolve(base)
	return &m, nil
}

// resolve walks the document once and fills in everything that is implied
// rather than stated: base URLs, period start and duration, and the effective
// segment addressing for each representation.
func (m *MPD) resolve(base string) {
	m.base = resolveBase(base, m.BaseURLs)
	m.resolvePeriodTimes()

	for _, p := range m.Periods {
		p.mpd = m
		p.base = resolveBase(m.base, p.BaseURLs)
		for _, a := range p.AdaptationSets {
			a.period = p
			a.base = resolveBase(p.base, a.BaseURLs)
			a.resolveContentType()
			for _, r := range a.Representations {
				r.set = a
				r.base = resolveBase(a.base, r.BaseURLs)
				r.inherit()
			}
		}
	}
}

// resolvePeriodTimes fills in Start and Duration for every period.
//
// @start is optional and @duration is optional, but not both at once for a
// period in the middle of a presentation: what is not written is implied by
// the neighbours. Start is resolved first, because a period's duration is
// often only knowable as the gap to the next period's start.
func (m *MPD) resolvePeriodTimes() {
	for i, p := range m.Periods {
		switch {
		case p.RawStart.Set:
			p.Start = p.RawStart.Value
		case i == 0:
			p.Start = 0
		default:
			// The previous period's end, when it stated its own length. If it
			// did not, we have nothing better than the previous start.
			prev := m.Periods[i-1]
			p.Start = prev.Start + prev.RawDuration.Or(0)
		}
	}
	for i, p := range m.Periods {
		switch {
		case p.RawDuration.Set:
			p.Duration = p.RawDuration.Value
		case i+1 < len(m.Periods):
			if d := m.Periods[i+1].Start - p.Start; d > 0 {
				p.Duration = d
			}
		case m.MediaPresentationDuration.Set:
			if d := m.MediaPresentationDuration.Value - p.Start; d > 0 {
				p.Duration = d
			}
		}
	}
}

// inherit folds the segment addressing declared above a representation into
// the representation itself.
//
// DASH lets SegmentTemplate appear at the period, adaptation set and
// representation levels, and the levels combine attribute by attribute rather
// than the lowest one replacing the rest: a period-level template commonly
// carries @media and @timescale while the representation supplies only its own
// SegmentTimeline. Merging here means every consumer sees one complete
// template instead of re-walking the tree.
func (r *Representation) inherit() {
	a, p := r.set, r.set.period

	if r.MimeType == "" {
		r.MimeType = a.MimeType
	}
	if r.Codecs == "" {
		r.Codecs = a.Codecs
	}
	if r.Width == 0 {
		r.Width = a.Width
	}
	if r.Height == 0 {
		r.Height = a.Height
	}
	if r.FrameRate == "" {
		r.FrameRate = a.FrameRate
	}

	r.SegmentTemplate = mergeTemplate(mergeTemplate(p.SegmentTemplate, a.SegmentTemplate), r.SegmentTemplate)
	if r.SegmentList == nil {
		if a.SegmentList != nil {
			r.SegmentList = a.SegmentList
		} else {
			r.SegmentList = p.SegmentList
		}
	}
	if r.SegmentBase == nil {
		if a.SegmentBase != nil {
			r.SegmentBase = a.SegmentBase
		} else {
			r.SegmentBase = p.SegmentBase
		}
	}
	// ContentProtection is declared at the adaptation set level by almost
	// every packager, and applies to the representations under it. A
	// representation that declares its own replaces the set's rather than
	// adding to it, which is the reading that keeps "this representation is
	// unprotected" meaning exactly that.
	if len(r.ContentProtections) == 0 {
		r.ContentProtections = a.ContentProtections
	}
}

// mergeTemplate returns the effective template for a child level: every
// attribute the child sets wins, everything else falls through to the parent.
// The result is a fresh value, so the merge never mutates a template shared by
// sibling representations.
func mergeTemplate(parent, child *SegmentTemplate) *SegmentTemplate {
	if parent == nil {
		return child
	}
	if child == nil {
		return parent
	}
	out := *parent
	if child.Media != "" {
		out.Media = child.Media
	}
	if child.Initialization != "" {
		out.Initialization = child.Initialization
	}
	if child.Index != "" {
		out.Index = child.Index
	}
	if child.Timescale != nil {
		out.Timescale = child.Timescale
	}
	if child.Duration != nil {
		out.Duration = child.Duration
	}
	if child.StartNumber != nil {
		out.StartNumber = child.StartNumber
	}
	if child.PresentationTimeOffset != nil {
		out.PresentationTimeOffset = child.PresentationTimeOffset
	}
	if child.AvailabilityTimeOffset != 0 {
		out.AvailabilityTimeOffset = child.AvailabilityTimeOffset
	}
	if child.AvailabilityTimeComplete != nil {
		out.AvailabilityTimeComplete = child.AvailabilityTimeComplete
	}
	if child.Timeline != nil {
		out.Timeline = child.Timeline
	}
	return &out
}

// resolveBase resolves the first BaseURL at a level against the base inherited
// from the level above.
//
// Only the first is used. Multiple BaseURL elements are alternates for CDN
// redundancy, not a list to fetch from at once; a prober that walked them all
// would multiply its request rate by the number of CDNs.
func resolveBase(parent string, urls []string) string {
	child := ""
	for _, u := range urls {
		if s := strings.TrimSpace(u); s != "" {
			child = s
			break
		}
	}
	if child == "" {
		return parent
	}
	return resolveRef(parent, child)
}

// resolveRef resolves ref against base, falling back to ref when base is empty
// or either side is unparsable -- a relative URL is more useful to a finding
// than an empty string.
func resolveRef(base, ref string) string {
	if base == "" {
		return ref
	}
	if ref == "" {
		return base
	}
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}
