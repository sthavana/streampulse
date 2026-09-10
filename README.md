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
| `discontinuity_present` | info | Discontinuity markers in window (ad-break awareness) |

Each finding is also counted in `streampulse_findings_total{check,severity,...}`.

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

## Metrics exposed (`/metrics`)

`streampulse_probe_up`, `streampulse_variant_up`, `streampulse_manifest_fetch_seconds`,
`streampulse_media_fetch_seconds`, `streampulse_media_sequence`,
`streampulse_playlist_window_seconds`, `streampulse_segment_count`,
`streampulse_segment_available`, `streampulse_segment_ttfb_seconds`,
`streampulse_variant_count`, `streampulse_findings_total`,
`streampulse_incident_active`, `streampulse_incidents_opened_total`,
`streampulse_incidents_resolved_total`.

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
- **DRM**: license-server reachability, key rotation gaps, PSSH sanity
- **Multi-vantage probing** (run from several regions; compare)
- **Cross-layer correlation**: map a QoE symptom to the offending layer
- **Maintenance windows** to suppress alerting during planned work
- **Web UI** over the incident state the tracker already keeps

## Status

MVP. HLS path is implemented and tested (`make test`). Not yet production-hardened.
