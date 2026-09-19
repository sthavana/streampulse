# HTTP API

Everything the prober and the multiviewer serve, and what you may rely on.

`/api/state` stopped being an implementation detail the moment the multiviewer
started reading it. It is now a contract with an external consumer, which is
why it has a stability statement rather than just a struct definition.

**All of these endpoints are unauthenticated and unencrypted.** See
[SECURITY.md](../SECURITY.md) before exposing any of them.

---

## Prober

Served on `metrics_addr` from the config, `:9090` by default.

| Method | Path | |
|---|---|---|
| GET | `/` | The operator page. HTML, embedded in the binary. |
| GET | `/api/state` | Everything the page renders, as JSON. |
| GET | `/metrics` | Prometheus text exposition. |
| GET | `/healthz` | `200 OK` with body `ok` once the server is listening. |
| GET | `/frame/{target}/{variant}` | The latest captured thumbnail, JPEG. Only when frame capture is on. |

### `GET /api/state`

One document describing everything the prober currently knows. Read it on a
timer; there is no streaming variant and no long-polling.

```json
{
  "now": "2026-09-19T08:31:04.881Z",
  "version": "v0.3.1",
  "vantage": "eu-west",
  "uptime_seconds": 40658,
  "summary": {
    "targets": 5, "targets_up": 4,
    "streams": 8, "streams_up": 8,
    "firing": 2, "suppressed": 0
  },
  "targets": [
    {
      "name": "unified-live-dash",
      "url": "https://demo.unified-streaming.com/.../.mpd",
      "format": "dash",
      "interval": "every 8s",
      "up": true,
      "live": true,
      "firing": 0,
      "metrics": {
        "probe_up": 1,
        "manifest_fetch_seconds": 0.184,
        "manifest_age_seconds": 4,
        "representation_count": 4
      },
      "trends": {
        "manifest_fetch_seconds": [0.19, 0.18, 0.21, 0.18]
      },
      "streams": [
        {
          "name": "video/video=1000000",
          "up": true,
          "live": true,
          "firing": 0,
          "metrics": {
            "segment_count": 311,
            "segment_ttfb_seconds": 0.174,
            "playlist_window_seconds": 594
          },
          "trends": { "segment_ttfb_seconds": [0.17, 0.18, 0.17] },
          "thumb": "/frame/unified-live-dash/video%2Fvideo%3D1000000?t=1758...",
          "audio": { "peak_dbfs": -3.1, "mean_dbfs": -18.4, "silent": false }
        }
      ]
    }
  ],
  "incidents": [
    {
      "severity": "critical",
      "check": "manifest_fetch",
      "target": "origin-unreachable",
      "variant": "",
      "message": "Get \"http://127.0.0.1:1/live.m3u8\": dial tcp ...",
      "count": 88,
      "first_seen": "2026-09-18T10:14:32Z",
      "last_seen": "2026-09-19T08:31:02Z",
      "announced": true
    }
  ],
  "recent": [ "…the last few notifications, newest first…" ]
}
```

**Fields worth explaining.**

| Field | Note |
|---|---|
| `now` | The prober's clock when it built this document. Compare it with your own to spot skew, which matters more than usual here — half the live-edge arithmetic in DASH depends on clocks agreeing. |
| `version` | The build serving it. Omitted by a build that does not know, which is any `go run`. |
| `vantage` | Omitted entirely when unset. Present, it is the region label on every metric too. |
| `targets[].up` | **Tri-state.** `true`, `false`, or absent — absent means not probed yet, which is not the same as down. Consumers must distinguish these; treating absent as down paints a fresh start as an outage. |
| `targets[].live` | Same tri-state rule. The multiviewer uses it to decide how to read the stream. |
| `metrics` | Metric names with the `streampulse_` prefix stripped. The set varies by format and by what has been observed; treat it as a bag, not a schema. |
| `trends` | Recent values, oldest first, for the four metrics that keep history. Absent when there are fewer than two readings. |
| `firing` | Count of open incidents for that target or stream, suppressed ones included. |
| `incidents[].announced` | **False** when a maintenance window is holding the notification quiet. The incident is still open and still reported here — suppression hides the page, not the fact. Note the polarity: `announced: false` is the suppressed one. |
| `incidents[].variant` | Omitted for a target-level incident. |
| `summary.suppressed` | How many open incidents are currently held quiet. |
| `thumb` | Carries a capture timestamp in the query string so a browser fetches the new picture rather than the one it has. |

Empty lists are `[]`, never `null` — the page and the wall both iterate them
directly.

### Stability

What you may rely on:

- **Fields are added, never removed or repurposed.** If a field's meaning has to
  change, it gets a new name.
- **A consumer must ignore fields it does not know.** The multiviewer parses a
  deliberate subset for exactly this reason.
- **Tri-state booleans stay tri-state.** `up` and `live` will not gain a
  default.

What you may not rely on:

- **The contents of `metrics` and `trends`.** Keys come and go with the checks.
  Read them defensively.
- **Ordering**, except that `targets` is sorted trouble-first — firing, then
  unreachable, then alphabetical — and `recent` is newest first. The wall
  inherits its tile order from this, which is why a broken stream appears at
  the top of both.

There is no version in the path. At this stage the honest statement is
additive-only rather than a version number that implies a migration policy I
have not committed to.

---

## Multiviewer

Served on `-addr`, `:9091` by default, under `-prefix` (`/mosaic`).

| Method | Path | |
|---|---|---|
| GET | `/mosaic/` | The wall. HTML, embedded. |
| GET | `/mosaic/api/targets` | The tile list: `[{"id","name"}]`. |
| GET | `/mosaic/events` | Server-sent events, one full status snapshot a second. |
| GET | `/mosaic/tile/{id}` | That tile's thumbnails as `multipart/x-mixed-replace` motion-JPEG. |

### `GET /mosaic/events`

Each message is `data: ` followed by a JSON array of tile statuses. The first
arrives immediately on connect rather than on the first tick, so a freshly
opened wall is not blank for a second.

```json
[
  {
    "target_id": "unified-live-dash",
    "name": "unified-live-dash",
    "severity": "critical",
    "summary": "timeline has not advanced for 41.0s (live edge frozen)",
    "up": true,
    "edge_age_s": 41,
    "ttfb_s": 0.174,
    "window_s": 594,
    "frame_age_s": 0.4,
    "stale": false
  }
]
```

| Field | Note |
|---|---|
| `severity` | `ok`, `info`, `warning` or `critical`, from the prober's worst open incident for that target. |
| `frame_age_s` | Seconds since the last thumbnail, or **-1** when none has ever arrived. Not the same as zero. |
| `stale` | The prober could not be reached; this tile's health is the last known answer rather than a current one. |

### `GET /mosaic/tile/{id}`

Standard motion-JPEG: `multipart/x-mixed-replace; boundary=frame`, one complete
JPEG per part. Renders in a plain `<img>` with no JavaScript.

Response headers are sent before the first frame, so a tile whose grabber has
not produced anything yet is a valid empty stream rather than a request hanging
with no response. An unknown id is `404`.

**A limit worth knowing:** each tile holds an HTTP connection open for as long
as it is displayed, and browsers allow about six per origin over HTTP/1.1. A
wall of more than six tiles wants HTTP/2, which means serving it over TLS.

---

## Prometheus metrics

`/metrics` on the prober, standard text exposition. The names are listed in the
[README](../README.md#metrics-exposed-metrics). Two properties worth relying on:

- **A removed target's series disappear.** They are not zeroed, they are
  dropped, because a series frozen at its last value is indistinguishable from
  an outage to any alert rule.
- **`vantage` is a constant label on every series** when configured, so several
  probers watching the same channels do not overwrite each other.
