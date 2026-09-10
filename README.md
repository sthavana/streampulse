# StreamPulse

Proactive, synthetic health monitoring for OTT / IPTV streaming — starting with HLS.

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
`streampulse_media_fetch_seconds`, `streampulse_media_sequence`,
`streampulse_playlist_window_seconds`, `streampulse_segment_count`,
`streampulse_segment_available`, `streampulse_segment_ttfb_seconds`,
`streampulse_variant_count`, `streampulse_rendition_count`,
`streampulse_findings_total`,
`streampulse_key_count`, `streampulse_key_available`,
`streampulse_key_fetch_seconds`,
`streampulse_incident_active`, `streampulse_incidents_opened_total`,
`streampulse_incidents_resolved_total`, `streampulse_maintenance_active`,
`streampulse_notifications_suppressed_total`.

## Quickstart

Requires Go 1.22+.

```bash
cp config.example.json config.json   # edit targets
make run
# metrics:   curl localhost:9090/metrics
# findings:  JSON lines on stdout
```

Point `config.json` at public test streams to try it. Apple's fMP4 bipbop
example works for the VOD path; Unified Streaming's `scte35.isml` demo is a
live channel with sparse PDT and ad markers, which exercises the freeze,
rollback and PDT rules. (Apple's older `bipbop_adv_example_hls` URL is dead
and now redirects to an HTML page -- StreamPulse flags it as
`unknown_playlist`, which is the correct result.)

## Architecture

```
                +-------------------+
   config.json  |      prober       |   goroutine per target, own ticker
  ------------> |  (cmd/prober)     |
                +---------+---------+
                          |
             +------------v------------+
             |   probe.ProbeTarget     |  fetch master -> variants -> media
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

Packages: `hls` (parser), `probe` (prober + checks), `metrics` (Prometheus
exposition), `alert` (findings + notifiers), `config` (targets).

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

- **DASH** MPD parsing (`$Number$`/`$Time$` templates, multi-period, availability window)
- **MPEG-TS / IPTV**: TR 101 290 P1/P2/P3-style checks (PCR jitter, CC errors, PAT/PMT integrity) via TSDuck
- **Deep segment inspection**: decode a frame (ffprobe) for black/freeze, codec/res vs declared, PTS continuity
- **Multi-vantage probing** (run from several regions; compare)
- **Cross-layer correlation**: map a QoE symptom to the offending layer
- **Web UI** over the incident state the tracker already keeps

## Status

MVP. HLS path is implemented and tested (`make test`). Not yet production-hardened.
