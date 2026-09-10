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

## Metrics exposed (`/metrics`)

`streampulse_probe_up`, `streampulse_variant_up`, `streampulse_manifest_fetch_seconds`,
`streampulse_media_fetch_seconds`, `streampulse_media_sequence`,
`streampulse_playlist_window_seconds`, `streampulse_segment_count`,
`streampulse_segment_available`, `streampulse_segment_ttfb_seconds`,
`streampulse_variant_count`, `streampulse_findings_total`.

## Quickstart

Requires Go 1.22+.

```bash
cp config.example.json config.json   # edit targets
make run
# metrics:   curl localhost:9090/metrics
# findings:  JSON lines on stdout
```

Point `config.json` at public test streams to try it (e.g. Apple's bipbop
examples for VOD; any of your live channels for the freeze/PDT checks).

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
         +--------v-----+   +-----v---------+
         | alert.Notifier|  | metrics.Registry|
         |  JSON / Slack |  |  /metrics (Prom)|
         +--------------+   +----------------+
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
- **Web UI + alert dedup / maintenance windows** to keep false positives low

## Status

MVP. HLS path is implemented and tested (`make test`). Not yet production-hardened.
