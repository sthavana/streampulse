# Running StreamPulse in production

This is the honest version. StreamPulse is a small, well-tested tool that has
been run against real streams throughout its development, but it has not been
run at scale by anyone, and the [limitations](#known-limitations) at the bottom
are real. Read those first if you are deciding whether to depend on it.

- [1. Get the code](#1-get-the-code)
- [2. Choose an image](#2-choose-an-image)
- [3. Write a config](#3-write-a-config)
- [4. Run it](#4-run-it) — [systemd](#systemd) · [Compose](#docker-compose) · [Kubernetes](#kubernetes)
- [5. Wire up alerting](#5-wire-up-alerting)
- [6. Security](#6-security)
- [7. Capacity](#7-capacity)
- [8. Operating it](#8-operating-it)
- [Known limitations](#known-limitations)

---

## 1. Get the code

**Fork it** if you expect to change anything — checks, thresholds, the UI, the
alert rules. You will want your changes on a branch you control, and you will
want to pull upstream fixes without a merge conflict in your own config.

```bash
git clone https://github.com/<you>/streampulse.git
cd streampulse
git remote add upstream https://github.com/sthavana/streampulse.git
```

**Clone it** if you are only going to run it. Pin to a tag rather than tracking
`main`, so an upstream change never arrives unannounced:

```bash
git clone --branch v0.1.0 https://github.com/sthavana/streampulse.git
```

Verify what you got before you run it anywhere that matters:

```bash
make check          # gofmt, go vet, tests under -race
make check-linux    # the same, in the environment CI uses
```

## 2. Choose an image

There are two, and the difference is not cosmetic.

| | Size | Contains | Use it when |
|---|---|---|---|
| `Dockerfile` | **10MB** | A static binary on `scratch`, plus a CA bundle | Default. Everything except media inspection |
| `Dockerfile.full` | **773MB** | The same binary on Debian with ffmpeg | You want codec/resolution checks, black/frozen/silent detection, or thumbnails |

```bash
docker build -t streampulse:v0.1.0 .                          # small
docker build -f Dockerfile.full -t streampulse:v0.1.0-full .  # with ffmpeg
```

The small image has no shell, no package manager and nothing in it but your
binary — you cannot `exec` into it, which is the point. Run it unless you
specifically need to look inside the media.

Both run as uid **65534** and serve `/healthz`.

## 3. Write a config

Start from `config.example.json`. A realistic production target:

```json
{
  "metrics_addr": "127.0.0.1:9090",
  "vantage": "eu-west-1",
  "reload_seconds": 10,

  "alerting": {
    "for_seconds": 30,
    "resolve_after_seconds": 120,
    "state_file": "/var/lib/streampulse/state.json"
  },

  "targets": [
    {
      "name": "channel-1",
      "url": "https://cdn.example.com/live/channel-1/master.m3u8",
      "type": "hls",
      "interval_seconds": 10,
      "max_variants": 2,
      "max_renditions": 1,
      "segment_sample": 1,
      "expect_live": true,
      "min_window_seconds": 30
    }
  ]
}
```

Four settings that matter more than the rest:

- **`metrics_addr`** — bind to `127.0.0.1` and reverse-proxy, or to a private
  interface. See [security](#6-security); this port has no authentication.
- **`for_seconds`** — how long a fault must persist before anyone is told. `0`
  pages on the first observation. `30` is a reasonable start: it swallows a
  single transient CDN 404 and costs you 30 seconds of detection latency.
- **`state_file`** — without it, a restart re-announces every open incident.
  With it, they survive. See the [volume ownership trap](#the-state-file-needs-a-writable-volume).
- **`vantage`** — set it if you will ever run more than one prober. It is
  cheaper to set now than to add later, when every dashboard query and alert
  rule has been written without it.

## 4. Run it

### systemd

The simplest deployment, and a good one: a 10MB static binary has very little
to go wrong.

```bash
make build
sudo install -m755 bin/prober /usr/local/bin/streampulse
sudo install -d -o nobody -g nogroup /var/lib/streampulse /etc/streampulse
sudo install -m644 config.json /etc/streampulse/config.json
```

```ini
# /etc/systemd/system/streampulse.service
[Unit]
Description=StreamPulse
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/streampulse -config /etc/streampulse/config.json
User=nobody
Group=nogroup
Restart=always
RestartSec=5

# It needs to make outbound HTTP requests and write one state file. Nothing else.
StateDirectory=streampulse
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
NoNewPrivileges=yes
RestrictAddressFamilies=AF_INET AF_INET6
SystemCallFilter=@system-service
CapabilityBoundingSet=

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now streampulse
journalctl -u streampulse -f    # findings are JSON lines on stdout
```

`PrivateTmp=yes` matters if you use media inspection: the prober writes
downloaded segments to a temp file before handing them to ffprobe.

### Docker Compose

`deploy/docker-compose.yml` is a working demo, not a production deployment —
it builds from source, has Grafana wide open with no login, and points at
public test streams. Use it as a starting point, not as-is.

```yaml
services:
  prober:
    image: streampulse:v0.1.0
    restart: unless-stopped
    ports:
      - "127.0.0.1:9090:9090"          # loopback only
    volumes:
      - ./config.json:/etc/streampulse/config.json:ro
      - ./state:/var/lib/streampulse   # see the ownership note below
    logging:
      driver: json-file
      options: { max-size: "10m", max-file: "5" }
```

#### The state file needs a writable volume

This one will bite you. The container runs as uid 65534, and a **fresh named
volume is owned by root**, so the prober cannot write its state file:

```
alert: could not write incident state: open /var/lib/streampulse/state.json.tmp… : permission denied
```

Nothing else breaks — every check keeps running — but incident state stops
surviving restarts, which is the whole reason you set it.

Use a bind mount you own:

```bash
mkdir -p state && sudo chown 65534:65534 state
```

or, with a named volume, chown it once before first use:

```bash
docker run --rm -v streampulse-state:/v alpine chown 65534:65534 /v
```

### Kubernetes

The same trap has a cleaner fix here: `fsGroup` makes the mounted volume
group-writable by the pod's user.

```yaml
apiVersion: apps/v1
kind: StatefulSet          # StatefulSet, not Deployment: the state file is per-prober
metadata:
  name: streampulse
spec:
  replicas: 1              # see the note on scaling below
  serviceName: streampulse
  selector:
    matchLabels: { app: streampulse }
  template:
    metadata:
      labels: { app: streampulse }
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9090"
    spec:
      securityContext:
        runAsUser: 65534
        runAsGroup: 65534
        fsGroup: 65534           # <- makes the volume writable by the prober
      containers:
        - name: prober
          image: streampulse:v0.1.0
          args: ["-config", "/etc/streampulse/config.json"]
          ports: [{ containerPort: 9090 }]
          livenessProbe:
            httpGet: { path: /healthz, port: 9090 }
            initialDelaySeconds: 5
          readinessProbe:
            httpGet: { path: /healthz, port: 9090 }
          resources:
            requests: { cpu: 50m, memory: 64Mi }
            limits:   { memory: 256Mi }
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
          volumeMounts:
            - { name: config, mountPath: /etc/streampulse }
            - { name: state,  mountPath: /var/lib/streampulse }
            - { name: tmp,    mountPath: /tmp }
      volumes:
        - { name: config, configMap: { name: streampulse-config } }
        - { name: tmp, emptyDir: {} }
  volumeClaimTemplates:
    - metadata: { name: state }
      spec:
        accessModes: ["ReadWriteOnce"]
        resources: { requests: { storage: 64Mi } }
```

`readOnlyRootFilesystem: true` works, but only with the `/tmp` `emptyDir`:
media inspection writes downloaded segments there.

**A ConfigMap update reloads without a restart.** Kubernetes remounts the
volume as a new symlink, the prober hashes the contents rather than watching
the inode, and it picks the change up on its next poll. Expect up to a minute
for the kubelet to propagate it, plus your `reload_seconds`.

**Do not scale the replica count to spread load.** Two replicas do not divide
the targets between them — they each probe everything, doubling your request
rate against the CDN and colliding in Prometheus unless they have different
`vantage` values. To probe from several places, run separate deployments with
distinct vantages; see [multi-vantage](../README.md#multi-vantage).

## 5. Wire up alerting

Scrape it and load the rules:

```yaml
scrape_configs:
  - job_name: streampulse
    static_configs:
      - targets: ["streampulse:9090"]
```

`deploy/prometheus/alerts.yml` is production-shaped and ready to copy. Two
rules cover every check, present and future, because the incident gauge carries
the check name and severity as labels:

```yaml
- alert: StreamPulseCritical
  expr: streampulse_incident_active{severity="critical"} == 1
```

Those rules carry almost no `for:` deliberately. StreamPulse already damps
flapping through `alerting.for_seconds`, and a second damper in Prometheus
means two places to reason about when something pages. **Do your damping in
the config**, not in the rules.

Route by severity in Alertmanager. `critical` means viewers are affected or
about to be; `warning` means something is wrong that is not yet an outage;
`info` is context, not a page.

## 6. Security

**The metrics and UI port is unauthenticated and must not be exposed
publicly.** It is read-only — there is no write API, deliberately — but it
reveals your stream topology, origin URLs, CDN behaviour and current faults.
Bind it to loopback or a private interface, and put a reverse proxy with
authentication in front if humans need the UI.

**Anyone who can edit the config can make this process fetch any URL.** That is
what it is for, and it is why there is no browser form for adding targets: a
prober is an SSRF engine by construction, and `streampulse_probe_up` alone
turns one into an internal port scanner. Treat config write access as
equivalent to shell access on the host.

**Mount the config read-only** (`:ro`). Nothing writes to it.

**Secrets.** Per-target `headers` can carry auth tokens, and `slack_webhook` is
a credential. Both live in the config file, so it should be a Kubernetes Secret
rather than a ConfigMap if it contains either. Findings never include header
values, and key material is never logged.

**Egress.** The prober needs outbound HTTPS to your CDNs and origins, and
nothing else. If you run it somewhere with egress rules, that is the whole
list.

## 7. Capacity

Measured, not estimated. One poll of one HLS target makes:

```
1 + P × (2 + S)   requests

  P = playlists probed (variants after max_variants, plus renditions)
  S = segment_sample
```

A target with 2 variants and 1 rendition at `segment_sample: 2` is **13
requests per poll** — 1 master, 3 media playlists, 3 initialisation segments,
6 segment samples. At a 10s interval that is 78 requests a minute, per target.

Three targets and six streams at 8–30s intervals used **13 MiB of memory and
effectively no CPU**. It is network-bound, not compute-bound — until you turn
on media inspection, which downloads whole segments and decodes them.

Rules of thumb:

- `segment_sample` is the biggest lever on request rate. `1` is usually enough:
  a CDN that is failing is rarely failing for exactly one segment.
- `max_variants: 2` and `max_renditions: 1` cover the ladder's extremes without
  probing all twelve rungs. For DASH, `max_representations: 1` probes the top
  rung of *every* track, which is usually what you want.
- `thumbnails` costs a full segment download and a decode per stream per poll.
  Budget for it, or raise those targets' intervals.
- Poll faster than the segment duration and you will see the same manifest
  twice, which is handled but wasteful. Interval ≈ segment duration is a good
  default.

## 8. Operating it

**Adding a stream** — edit the config and save. It takes effect within
`reload_seconds`, no restart, and untouched targets are not disturbed. A config
that fails to parse or validate is logged and ignored; the running one stays in
force.

**Upgrading** — pull the new tag, rebuild, restart. With `state_file` set, open
incidents survive and nobody is re-paged for a fault they already know about.
Read the release notes for metric or check renames before upgrading a
deployment whose dashboards you care about.

**What to back up** — nothing. The config is in your git repo, and the state
file is a cache that rebuilds itself. Prometheus holds the history.

**Logs** — findings are JSON lines on stdout, one per incident transition.
Ship them wherever your logs go; they are structured and stable.

**Watching the watcher** — `up{job="streampulse"} == 0` and
`StreamPulseProberDown` are in the shipped rules. Monitoring that is down and
silent looks exactly like everything being fine.

## Known limitations

Stated plainly, because you are deciding whether to depend on this.

- **Not run at scale by anyone.** It has been validated continuously against
  live streams from Unified Streaming, Apple and Akamai, but the largest
  deployment to date is a handful of targets.
- **One prober, one process.** There is no clustering and no work sharing.
  Scaling means more probers with different `vantage` values.
- **`frozen_video` is off by default** because nothing inside a segment
  distinguishes a frozen encoder from a channel legitimately showing a slate.
  Turn it on per deployment, knowing your content.
- **TR 101 290 coverage is Priority 1 only**, and only the part answerable from
  an HTTP segment. The timing measurements need a continuous transport stream.
- **No authentication anywhere.** Network placement is the entire access
  control story.
