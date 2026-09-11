# StreamPulse

[![CI](https://github.com/sthavana/pulsemark/actions/workflows/ci.yml/badge.svg)](https://github.com/sthavana/pulsemark/actions/workflows/ci.yml)

Proactive, synthetic health monitoring for OTT / IPTV streaming — HLS and DASH.

StreamPulse continuously pulls your manifests and segments the way a player would,
parses them, and flags problems **before** viewers complain: frozen live edges,
stalled playlists, segments that 404 on the CDN, spec violations, and more. It
exposes Prometheus metrics and streams structured findings (stdout JSON, Slack).

> Working codename — check trademark availability before using it commercially.

## Why

Most streaming monitoring is passive (ingest logs, wait for QoE dashboards to dip)
and single-layer. The expensive, unsolved problem operators actually have is
**catching a break at the manifest/segment layer early, and knowing which layer
caused it**. StreamPulse is a synthetic prober built around that: it *actively*
validates the stream from wherever it runs, on a tight schedule.

The near-term wedge: **self-hostable** (no shipping stream data to a SaaS),
**dependency-light**, and **API/metrics-first** so it drops straight into an
existing Prometheus + Grafana + Alertmanager stack.

## What it checks today (HLS)

Variants and `EXT-X-MEDIA` renditions alike -- the playlist checks below run
against alternative audio and subtitle tracks as well as the video ladder.


| Check | Severity | What it catches |
|---|---|---|
| `manifest_fetch` / `media_fetch` | critical | Origin/CDN unreachable or non-200 |
| `segment_availability` | critical | Manifest references a segment that 404s / errors |
| `playlist_stalled` | critical | Live media sequence not advancing (frozen edge) |
| `playlist_rollback` | critical | Media sequence went backwards (stale origin / failover) |
| `no_segments` | critical | Playlist parsed but empty |
| `unexpected_endlist` | critical | `EXT-X-ENDLIST` on a stream you declared live |
| `targetduration_violation` | warning | Segment longer than `EXT-X-TARGETDURATION` (RFC 8216) |
| `pdt_not_advancing` | warning | Window slid but `PROGRAM-DATE-TIME` did not follow |
| `pdt_stale` | warning | Projected live edge is behind wall-clock |
| `short_window` | warning | Live/DVR window shorter than expected |
| `targetduration_missing` | warning | Missing required tag |
| `init_segment_availability` | critical | `EXT-X-MAP` initialisation section 404s -- playback cannot start at all |
| `map_missing_uri` | critical | `EXT-X-MAP` with no `URI`, which the spec requires |
| `rendition_group_missing` | critical | Variant references an AUDIO/SUBTITLES/VIDEO group no `EXT-X-MEDIA` declares |
| `rendition_duplicate_name` | warning | Two renditions in one group share a `NAME` (RFC 8216 4.3.4.1.1) |
| `rendition_multiple_default` | warning | More than one `DEFAULT=YES` in a group |
| `closed_captions_with_uri` | warning | `CLOSED-CAPTIONS` rendition carries a `URI`, which the spec forbids |
| `unexpected_clear_segments` | critical | Segments with no `EXT-X-KEY` on a stream declared encrypted |
| `key_fetch` | critical | Key URI unreachable or non-200 |
| `key_size_invalid` | critical | Identity key URI returned something other than 16 bytes |
| `key_missing_uri` | critical | `EXT-X-KEY` with a method but no `URI` |
| `key_invalid_iv` | warning | `IV` is not `0x` + 32 hex digits |
| `key_rotation_stalled` | warning | Key unchanged for longer than the expected rotation interval |
| `pssh_malformed` / `pssh_empty` | warning | Embedded PSSH box fails to parse or carries nothing |
| `discontinuity_present` | info | Discontinuity markers in window (ad-break awareness) |

Each finding is also counted in `streampulse_findings_total{check,severity,...}`.

## Renditions (EXT-X-MEDIA)

Alternative audio, subtitle and video-angle tracks are declared with
`EXT-X-MEDIA`, separately from the `EXT-X-STREAM-INF` ladder. A tool that walks
only the variants will report a stream as perfectly healthy while its audio is
dead -- which is one of the more common real-world breaks.

Renditions carrying a `URI` are probed with the same rules as any media
playlist: reachability, stall and rollback, PDT progression, segment
availability and TTFB. They are declared once at the master level, so they are
fetched once per cycle no matter how many variants reference them.

A rendition with **no** `URI` is skipped, and that absence is meaningful rather
than a defect: audio without a URI is muxed into the variant streams, and for
`CLOSED-CAPTIONS` the URI *must not* be present at all -- captions ride inside
the video segments. Unified Streaming's live demo declares its audio this way;
Apple's fMP4 example ships separate audio and subtitle playlists.

Renditions are labelled `type/group/name`, e.g. `audio/aud1/English`. The group
is part of the label because `NAME` alone is not unique: Apple's own reference
stream declares three audio renditions all named "English" (a stereo mix and
two 5.1 mixes) in groups `aud1`/`aud2`/`aud3`. Labelling them by name alone
would collapse three independent tracks into one metric series and one
incident, masking a break in any of them.

Two per-target knobs, both optional:

```json
"max_renditions": 0,              // 0 = all renditions that have a URI
"rendition_types": ["AUDIO"]      // omit for every type
```

`rendition_types` is the one to reach for on a stream carrying thirty subtitle
languages you do not need to probe every few seconds.

## Initialisation sections (EXT-X-MAP)

Every fMP4 stream carries an `EXT-X-MAP`: the initialisation section a player
loads before it can decode anything. Nothing else in the playlist references
it, so when it goes missing the manifest still parses, every media segment
still serves, and playback simply never starts. It gets a request of its own
for that reason, and unlike a segment at the live edge it is static, so a 404
there is unambiguous.

Each distinct section is fetched once per cycle however many segments
reference it. A playlist has more than one when the initialisation section
changes mid-stream, which happens at a discontinuity between differently
packaged sources. A `BYTERANGE` section is probed *at its declared offset*
rather than at byte zero, so a file truncated before the part that matters is
caught instead of passed.

`EXT-X-MAP` and a DASH `Initialization` are the same object under two names,
and both formats run the same check through the same code: a stream is not
judged differently for saying it in a different dialect. Exported as
`streampulse_init_segment_available`.

## DRM and EXT-X-KEY

Key problems are among the most expensive stream failures: playback stops dead
for every viewer at once, and the manifest and segments all look fine.
StreamPulse parses `EXT-X-KEY` and `EXT-X-SESSION-KEY`, tracks which key
applies to which segment, and optionally proves the key is actually retrievable.

```json
"expect_encrypted": true,
"fetch_keys": true,
"key_rotation_max_seconds": 3600
```

**`expect_encrypted`** catches clear-lead leakage: segments going out
unprotected on a stream that is supposed to be encrypted. Nothing 404s and the
stream plays perfectly, which is exactly why it goes unnoticed. `METHOD=NONE`
is parsed as what it is -- a deliberate return to clear -- so a mid-playlist
switch is detected rather than ignored.

**`fetch_keys`** retrieves each distinct key URI, as a player would. For an
`identity` KEYFORMAT (plain AES-128) the response must be exactly 16 bytes; a
key endpoint serving an HTML error page with HTTP 200 is a real failure mode
that a reachability check alone would pass.

> Key material is never written to a finding, a log line, or a notification.
> Findings carry the URI, the DRM system, and byte counts only.

Not every key URI is an HTTP resource, and those are skipped rather than
reported as failures: FairPlay uses `skd://`, and Widevine and PlayReady
usually embed initialisation data in a `data:` URI. Embedded payloads carrying
a `pssh` box are parsed and validated (system ID, KIDs, size fields); a payload
that is *not* a PSSH box is left alone, because PlayReady commonly ships a bare
PlayReady Object and flagging it would be a false positive.

Relative key URIs (`URI="key.bin"`) resolve against the media playlist, which
is how most packagers write them.

**`key_rotation_max_seconds`** flags a live stream whose keys have stopped
rotating. It is opt-in because plenty of streams legitimately never rotate.

## DASH

Point a target at an `.mpd` and it is probed: the manifest is fetched and
parsed, every representation is enumerated, and the most recent segments of
each are fetch-checked. Findings go through the same incidents, maintenance
windows and metrics as the HLS path.

| Check | Severity | What it catches |
|---|---|---|
| `manifest_fetch` | critical | Origin/CDN unreachable or non-200 |
| `manifest_parse` | critical | Response is not a parsable MPD (a CDN error page served with a 200) |
| `manifest_empty` | warning | MPD parsed but declares no representations |
| `no_segments` | critical | No segment is available in the current window |
| `segment_availability` | critical | A segment the MPD points at 404s / errors |
| `playlist_stalled` | critical | Timeline not advancing (frozen live edge) |
| `playlist_rollback` | critical | Timeline went backwards (stale origin / failover) |
| `short_window` | warning | Live/DVR window shorter than expected |
| `unexpected_static` | critical | `type="static"` on a stream you declared live |
| `edge_stale` | warning | Live edge falling behind wall-clock (encoder losing ground) |
| `init_segment_availability` | critical | Initialisation segment 404s -- playback cannot start at all |
| `unexpected_clear_segments` | critical | No `ContentProtection` on a stream declared encrypted |
| `pssh_malformed` / `pssh_empty` | warning | `cenc:pssh` fails to parse or carries nothing |
| `kid_invalid` | warning | `default_KID` is not a key id |
| `discontinuity_present` | info | More than one period (each boundary is a splice point) |
| `chunked_delivery_missing` | warning | A low-latency stream is being delivered as whole buffered segments |
| `utc_timing_missing` | warning | Low-latency stream declares no `UTCTiming` |

`edge_stale` is the DASH counterpart of HLS's `pdt_stale`. The names differ
because `pdt_stale` names a tag DASH does not have; they could be unified under
one name later, but renaming a check breaks existing alert rules, so it is left
as a deliberate decision rather than done quietly.

### Low latency

A chunked low-latency stream declares itself two ways, and either is enough
because packagers are inconsistent about which they emit: a
`ServiceDescription/Latency` target, or a `SegmentTemplate` with
`availabilityTimeComplete="false"` and an offset. An offset on its own is not
low latency -- it means whole segments published slightly early, which is a
different thing and is not judged by these rules.

**`chunked_delivery_missing` is the one worth having.** If the packager, or any
proxy in front of it, buffers each segment and only answers once it is
complete, nothing appears to break: the manifest is right, every segment
serves, players play. They just play seconds behind where the design says, and
the entire low-latency build is inert. No other check can see it, because
nothing is wrong with any individual response -- only with when it started
arriving.

The signal is the response declaring its own length. A segment still being
produced cannot have a known length, so a `Content-Length` on one means
something waited for the whole thing. That requires a plain GET: the range
request every other probe uses always comes back with a length, whatever the
origin would have done otherwise. Only a segment whose production has not
finished can answer the question, so a poll that finds none simply says
nothing rather than judging a completed segment for having a length it is
entitled to.

**Latency bounds tighten the staleness check.** A stream that declares
`Latency@max` has said what "too far behind" means for it, and that replaces
the generic 30s floor -- which, on a three-second target, is not conservative
but blind. `edge_stale` names the declared bound in its message.

That measurement is taken from when a segment *finishes* being produced, not
from when it becomes fetchable. The two differ by exactly
`@availabilityTimeOffset` on a low-latency stream, and measuring from the
earlier one would charge the offset against the stream twice -- reporting
something inside its latency budget as failing it, on every poll.

`UTCTiming` is required of these streams and not of ordinary ones: low-latency
playback is anchored to wall clock, and at a three-second target a few seconds
of client drift is the entire budget. An ordinary stream has seconds of buffer
to absorb the same drift.

Exported as `streampulse_chunked_delivery`.

### DRM

`expect_encrypted` works, but the key family does not port. An `EXT-X-KEY`
carries a URI a player fetches, so StreamPulse can prove the key is
retrievable; an MPD carries no such thing. Licence acquisition is a
DRM-system protocol with a signed challenge, which no synthetic prober can
stand in for. What is checkable is what the manifest asserts: that the content
is protected at all, and that the initialisation data it ships is well formed.
`ContentProtection` declared on an adaptation set is inherited by its
representations, so "this representation is unprotected" means exactly that.

The init segment gets a request of its own because its failure mode is
invisible to every other check: the manifest parses, every media segment is
served, and playback still cannot start, because a player fetches the init
segment first and has nothing to initialise the decoder with.

### Where the freeze check applies, and why it does not always

`playlist_stalled` compares the **manifest-declared** live edge across polls,
and that is a narrower thing than it sounds:

- On a **SegmentTimeline** the manifest states where the edge is. A packager
  that stops publishing serves a byte-identical timeline on the next poll, so
  the freeze is visible in the manifest and the check fires.
- On a **number-addressed template** (`$Number$` with `@duration`) the manifest
  states no such thing. It is a formula, and the client extrapolates the edge
  from its own clock -- so a computed edge advances whether or not the packager
  is still alive. Comparing it would be comparing our clock to itself. It could
  never fire, which is worse than not checking: on a dashboard it would look
  exactly like coverage.

Those streams are not left uncovered, the coverage just lives somewhere else.
The segment sample asks the CDN for the segment at the computed edge on every
poll, and a packager that has stopped publishing fails that fetch:
`segment_availability` **is** the freeze check for number-addressed DASH. This
is why `segment_sample` matters more on a DASH target than on an HLS one.

`edge_stale` is deliberately coarse -- a 30s floor. Two things that are not
faults live in that number: a packager cannot publish a segment before it has
finished producing it, so a healthy edge sits a few segments back by
construction (Unified Streaming's demo channel runs ~6s behind with 1.92s
segments), and the comparison is against *our* clock, so NTP skew on the probe
host lands here too. What it is for is an encoder falling progressively behind,
which nothing else sees -- the timeline still advances and the segments all
exist -- and that fault grows to minutes, so a coarse threshold loses nothing.

The tolerance before an unchanged edge counts as frozen is three segments,
floored at 6s and raised by `@minimumUpdatePeriod`: a manifest that says it
republishes every 30s is *supposed* to serve an identical timeline for 30s at a
time, and judging it by its segment duration alone would report a healthy
stream as frozen on almost every poll.

A period that declares its own end is skipped too -- the completed period
before an ad break is *supposed* to have a static timeline, and reporting it
would page someone at every ad break on the channel.

Parsing an MPD is a different job from parsing a playlist. Almost nothing a
prober needs is stated outright: segment URLs live in templates, period start
times are usually implied by the periods around them, `BaseURL` and
`SegmentTemplate` are inherited down four levels, and on a live stream *which
segments exist at all* is a function of wall-clock time. `dash.Parse` resolves
all of it up front, so a caller gets representations that know their own base
URL, their own effective template, and how to enumerate segments:

```go
m, err := dash.Parse(body, manifestURL)
for _, r := range m.Representations() {
    for _, seg := range r.SegmentsAt(time.Now()) {
        // seg.URI is fully resolved and fetchable
    }
}
```

Three per-target knobs:

```json
"type": "dash",                          // omit to detect from the response body
"max_representations": 1,                // per adaptation set; 0 = all
"representation_types": ["video","audio"]
```

`max_representations` caps **per adaptation set**, not per manifest, and keeps
the highest-bandwidth rungs. A flat cap over a flattened ladder would truncate
wherever the packager happened to put the boundary: on a document listing six
video rungs before its audio, "max 3" would stop probing audio entirely --
exactly the break this tool exists to catch. Per set, `1` means "the top rung
of every track".

`type` is optional because the response body is unambiguous, but naming it
turns "unrecognized" into a parse error that says what was actually served.

What is handled:

| | |
|---|---|
| Addressing | `SegmentTemplate` with `$Number$` or `SegmentTimeline`, `SegmentList`, `SegmentBase` |
| Identifiers | `$Number$`, `$Time$`, `$RepresentationID$`, `$Bandwidth$`, `$$`, and the `%0Nd` padding tag |
| Timelines | `@t`/`@d`/`@r`/`@n`, including `@r="-1"` runs and mid-timeline gaps |
| Multi-period | `@start` and `@duration` inferred from the neighbouring periods and `@mediaPresentationDuration` |
| Inheritance | `BaseURL` chains and attribute-wise `SegmentTemplate` merging across MPD / Period / AdaptationSet / Representation |
| Timing | `xs:duration` and `xs:dateTime`, `@presentationTimeOffset`, `@timescale`, `@availabilityTimeOffset` |
| DRM | `ContentProtection`, `@default_KID`, and the `cenc:pssh` payload as raw bytes |

`SegmentsAt(t)` returns the **availability window**, not every segment the
manifest could describe: on a dynamic MPD that is the segments that have
finished being produced (their end is at or before the live edge) and have not
yet aged out of `@timeShiftBufferDepth`. That distinction is the whole point of
computing it. Asking a CDN for a segment the packager has not published yet is
a 404 that means nothing, and a prober that reported it would page someone for
a healthy stream.

Enumeration is arithmetic rather than iterative, and capped. A channel that has
been live for a week is hundreds of thousands of segments deep, and only the
last few minutes of it are fetchable.

One shape difference from HLS is worth naming: an MPD is a single document
that already describes every representation, so there is no second fetch per
rung of the ladder. That is why `streampulse_variant_up` and
`streampulse_media_fetch_seconds` have no DASH equivalent -- there is no
per-representation manifest to be up -- while `streampulse_media_sequence`
does: the newest segment number is the live-edge position, and it advances the
same way `EXT-X-MEDIA-SEQUENCE` does.

Where the spec leaves room, the parser takes the tolerant reading and leaves
the judgement to the check layer: a malformed `@suggestedPresentationDelay`
leaves that one attribute unset rather than costing the whole manifest, and a
dynamic MPD with no `@availabilityStartTime` is read as fully available rather
than as empty. Deciding that either is *wrong* is a check's job, not a parser's.

## Which layer broke: cache attribution

A frozen manifest has two very different causes that look identical from a
prober: the **packager** stopped producing, or a **CDN edge** is serving a
stale cached copy of a manifest the origin is still updating. Both are real
faults -- players hitting that edge see the same frozen stream -- but they are
different teams' problems at 3am, and "the live edge is frozen" does not say
which.

The answer is usually sitting in the response headers, so StreamPulse reads
them and puts the verdict in the finding:

```
playlist_stalled  media sequence has not advanced for 42.0s (live edge frozen)
                  [cache: Age 1.0s, MISS -- fresher than the freeze, so the origin is serving it]

playlist_stalled  timeline has not advanced for 44.0s (live edge frozen)
                  [cache: Age 120.0s, HIT -- old enough to account for the freeze; check the origin before the packager]
```

The reasoning is only as strong as the `Age` header, and is worded as evidence
rather than a verdict. If the response we just read is *younger* than the
freeze, the origin generated this frozen manifest moments ago and the packager
is at fault. If it is at least as old as the freeze, one stale cached object
could account for everything we have seen. An edge that says outright it is
past its TTL (nginx's `STALE` and `UPDATING`) settles it by itself.

Hit/miss is read from whichever header the CDN uses -- `X-Cache`, `X-Cached`,
`CF-Cache-Status`, `X-Cache-Status` -- normalised to HIT / STALE / MISS. In a
CDN chain (`X-Cache: HIT, MISS`) only the last entry counts: that is the edge
that actually served us. Not every CDN says anything at all -- Akamai strips
these by default -- and when nothing is said, nothing is claimed.

Exported as `streampulse_manifest_age_seconds` and
`streampulse_manifest_cache_hit`. Age is the one worth graphing: a step change
in it is an edge that stopped revalidating, and it shows up well before
anything times out.

Two per-target knobs:

```json
"no_cache": false,                       // send Cache-Control: no-cache
"headers": { "X-Auth": "token" }         // extra request headers
```

`no_cache` is **off** by default, and deliberately so. Bypassing the edge would
measure the origin, but viewers do not watch the origin: a stale edge is a real
outage, and a prober that never sees one is measuring the wrong thing. Turn it
on for a *second* target aimed past the cache, and compare the two -- that pair
is the cleanest origin-vs-edge signal available without instrumenting the CDN.

Probe requests identify themselves as `StreamPulse/0.1 (synthetic prober)`, so
they can be separated from real viewers in an origin access log.

## Alerting: incidents, not findings

A prober re-observes a fault on every poll. Sent straight to Slack, one frozen
playlist on a 4s interval is ~900 messages an hour, and a channel that noisy
gets muted -- which is the actual failure mode behind most alert fatigue.

Findings are therefore deduplicated into **incidents**, keyed by
`(target, variant, check)`. You get one notification when an incident opens and
one when it clears, regardless of how many polls observed it in between. The
message is kept fresh from the latest observation but is deliberately not part
of the key, so a check like `segment_availability` naming a different segment
URI each poll still collapses to one incident.

```json
"alerting": {
  "for_seconds": 0,            // condition must persist this long before notifying
  "resolve_after_seconds": 90, // quiet period before an incident is declared cleared
  "repeat_every_seconds": 0,   // re-notify while still firing; 0 = off
  "sweep_seconds": 10          // how often expiry is checked
}
```

```json
"alerting": { "state_file": "/var/lib/streampulse/state.json" }
```

`state_file` carries open incidents across a restart. Without it a redeploy
re-announces every firing fault, paging about things everyone was already told
about -- the exact fatigue the deduplication exists to prevent. It is opt-in
because it needs somewhere writable to live, and that is a deployment decision
rather than something to guess at.

What is restored is the incident's history and the fact that it was already
announced, not its last-seen time. That is deliberate: the tracker's model is
"presumed firing until `resolve_after_seconds` passes with no observation", and
after a restart there have been no observations. Restoring the old timestamp
would make the first sweep resolve everything instantly -- announcing a
clearing for faults that are probably still live, then re-opening them a poll
later. Each restored incident gets a full grace period instead. Still broken,
and the next poll refreshes it in silence; fixed while the process was down,
and it resolves properly, once.

A snapshot older than an hour is ignored: the point is surviving a restart, and
beyond that the world has moved on. A corrupt or unreadable one is logged and
stepped over, because monitoring that refuses to start over its own scratch
file is worse than monitoring with no memory.

`for_seconds` is the flap damper: raise it and a transient CDN 404 that clears
on the next poll is never announced at all. It trades that much detection
latency for silence, so it is 0 by default.

Resolution is by expiry rather than by observing a clean evaluation, because a
probe that fails early (an unreachable manifest) never evaluates the downstream
playlist checks that cycle -- "evaluated and clean" would wrongly clear them.

Incident state is exported too: `streampulse_incident_active` is 1 while firing
and 0 once cleared, alongside `streampulse_incidents_opened_total` and
`streampulse_incidents_resolved_total`.

## Maintenance windows

Planned work should not page anyone. Windows suppress alerting for a scope you
choose, either recurring or one-off:

```json
"maintenance": [
  {
    "name": "nightly encoder restart",
    "targets": ["channel-1"],          // omit for all targets
    "checks": ["playlist_stalled"],    // omit for all checks
    "timezone": "America/Los_Angeles",
    "daily": { "start": "02:00", "end": "04:00", "days": ["Sat", "Sun"] }
  },
  {
    "name": "packager upgrade",
    "start": "2026-09-15T22:00:00Z",
    "end": "2026-09-16T02:00:00Z"
  }
]
```

Windows are expressed in wall-clock time in their own `timezone` (default UTC),
so a 02:00 window stays at 02:00 local across a DST change rather than drifting
an hour. `daily` windows may cross midnight (`22:00`-`02:00`); when they do,
`days` refers to the day the window *opened*, so a Saturday window covers Sunday
01:00. The timezone database is embedded in the binary, so this works on a
scratch container with no system tzdata.

What suppression does and does not do:

- A fault that opens **and** clears entirely inside a window is never announced.
- A fault that opens inside a window and is **still open when the window closes**
  is announced then, carrying its full observation count -- so an operator can
  see it did not just start. This is the "did I break something?" case, and it
  is why findings are still tracked rather than dropped.
- A **resolve** for an incident announced *before* the window is always
  delivered, even mid-window. A resolve is never a page, and withholding it
  would leave someone believing a fault they were told about is still open.

Suppression is deliberately visible rather than silent:
`streampulse_maintenance_active{window}` is 1 while a window is open, and
`streampulse_notifications_suppressed_total{window,target,check}` counts what
was withheld. A malformed window (bad timezone, bad clock, both forms at once)
fails at startup rather than being ignored -- a window that silently never
matches is worse than one that refuses to load.

## Metrics exposed (`/metrics`)

`streampulse_probe_up`, `streampulse_variant_up`, `streampulse_manifest_fetch_seconds`,
`streampulse_manifest_age_seconds`, `streampulse_manifest_cache_hit`,
`streampulse_media_fetch_seconds`, `streampulse_media_sequence`,
`streampulse_playlist_window_seconds`, `streampulse_segment_count`,
`streampulse_segment_available`, `streampulse_segment_ttfb_seconds`,
`streampulse_variant_count`, `streampulse_rendition_count`,
`streampulse_period_count`, `streampulse_representation_count`,
`streampulse_findings_total`,
`streampulse_key_count`, `streampulse_key_available`,
`streampulse_init_segment_available`, `streampulse_chunked_delivery`,
`streampulse_stream_live`,
`streampulse_key_fetch_seconds`,
`streampulse_incident_active`, `streampulse_incidents_opened_total`,
`streampulse_incidents_resolved_total`, `streampulse_maintenance_active`,
`streampulse_notifications_suppressed_total`.

## Quickstart

Requires Go 1.22+.

```bash
cp config.example.json config.json   # edit targets
make check                           # gofmt + vet + tests with -race, what CI runs
make run                             # or: make compose-up for the full stack
# metrics:   curl localhost:9090/metrics
# findings:  JSON lines on stdout
```

Point `config.json` at public test streams to try it. Apple's fMP4 bipbop
example works for the VOD path; Unified Streaming's `scte35.isml` demo is a
live channel with sparse PDT and ad markers, which exercises the freeze,
rollback and PDT rules. (Apple's older `bipbop_adv_example_hls` URL is dead
and now redirects to an HTML page -- StreamPulse flags it as
`unknown_playlist`, which is the correct result.)

## Running it

### Docker

```bash
docker build -t streampulse .
docker run --rm -p 9090:9090 \
  -v "$PWD/config.json:/etc/streampulse/config.json:ro" \
  streampulse
```

The image is a static binary on `scratch` plus a CA bundle -- a few megabytes,
no shell, no package manager, nothing else in it that can carry a CVE. That is
a direct dividend of the zero-dependency stance: the timezone database is
already compiled in (`time/tzdata`), so maintenance windows resolve their
locations with no system tzdata to install.

It runs as uid 65534 and serves `/healthz` for an orchestrator's probe. There
is no `HEALTHCHECK` in the image because `scratch` has no shell to run one
with.

### The whole stack

```bash
make compose-up     # or: docker compose -f deploy/docker-compose.yml up --build
```

Brings up the prober, Prometheus and Grafana against public test streams --
Unified Streaming's live channel in **both** HLS and DASH, which is the same
content through both code paths side by side, plus Apple's fMP4 VOD.

| | |
|---|---|
| Grafana | http://localhost:3000 (anonymous, no login) |
| Prometheus | http://localhost:9091 |
| prober metrics | http://localhost:9090/metrics |
| findings | `docker compose -f deploy/docker-compose.yml logs -f prober` |

Edit `deploy/config.json` for your own streams and
`docker compose restart prober`.

### Alerting

`deploy/prometheus/alerts.yml` maps the checks to Prometheus alerts. Two rules
cover every check there is, present and future, because the incident gauge
carries the check name and severity as labels:

```yaml
- alert: StreamPulseCritical
  expr: streampulse_incident_active{severity="critical"} == 1
```

Those rules deliberately carry almost no `for:`. StreamPulse already damps
flapping -- findings are deduplicated into incidents, and
`alerting.for_seconds` decides how long a fault must persist before an incident
opens at all. A second `for:` in Prometheus would mean two places to reason
about when something pages, and two places to get it wrong.

The rest of the rules cover what the incident machinery cannot: the prober
being unscrapeable, and two symptoms worth seeing before they become faults
(a climbing manifest cache age, and segment TTFB).

### Verified

The stack in `deploy/` has been run end to end: the image builds (9.3MB),
probes real streams over HTTPS from `scratch`, Prometheus scrapes it, all six
alert rules load, Grafana provisions its datasource and dashboard, and a
deliberately broken target produces a firing `StreamPulseCritical` carrying the
check name and target as labels.

### Known rough edge

Metric series are never deleted. Remove a target from the config while one of
its incidents is firing, and `streampulse_incident_active` for it stays at 1
until the process restarts -- so the alert stays up with nothing behind it.
Restarting the prober clears it.

## Architecture

```
                +-------------------+
   config.json  |      prober       |   goroutine per target, own ticker
  ------------> |  (cmd/prober)     |
                +---------+---------+
                          |
             +------------v------------+
             |   probe.ProbeTarget     |  HLS: master -> variants -> media
             |                         |  DASH: MPD -> representations
             |   + runChecks (state)   |  + sample recent segments
             +----+---------------+----+
                  |               |
        findings  |               |  metrics
       +---------v---------+ +-----v---------+
       |  alert.Tracker    | | metrics.Registry|
       | dedup -> incident | |  /metrics (Prom)|
       +---------+---------+ +----------------+
                 | open / resolve only
         +-------v------+
         | alert.Notifier|
         |  JSON / Slack |
         +--------------+
```

Packages: `hls` and `dash` (manifest parsers), `probe` (prober + checks),
`metrics` (Prometheus exposition), `alert` (findings + notifiers), `config`
(targets).

## Deliberate design choices

- **Zero external dependencies (stdlib only).** Compiles in seconds anywhere,
  trivial to audit, nothing to CVE-patch. The HLS parser, the Prometheus
  exposition, and the notifiers are all small and self-contained.
- **Clear swap points for scale.** When dependencies are warranted:
  the parser → `grafov/m3u8`; metrics → `prometheus/client_golang`;
  config → YAML; notifiers → add PagerDuty/webhook/DB sinks. None of these
  change the interfaces the prober uses.
- **State lives in the prober**, keyed by playlist URL, so cross-poll checks
  (freeze, PDT progression) work without external storage for the MVP.

## Roadmap

- **MPEG-TS / IPTV**: TR 101 290 P1/P2/P3-style checks (PCR jitter, CC errors, PAT/PMT integrity) via TSDuck
- **Deep segment inspection**: decode a frame (ffprobe) for black/freeze, codec/res vs declared, PTS continuity
- **Multi-vantage probing** (run from several regions; compare)
- **Cross-layer correlation**: map a QoE symptom to the offending layer
  (started: manifest findings already carry an origin-vs-edge verdict)
- **Web UI** over the incident state the tracker already keeps

## Status

MVP. The HLS path is implemented and tested end to end (`make test`). DASH is
probed for reachability, segment availability, live-edge progression and the
DRM its manifest declares. Not yet production-hardened.
