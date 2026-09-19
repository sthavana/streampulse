# Security

## Reporting a vulnerability

Open a [private security advisory](https://github.com/sthavana/streampulse/security/advisories/new)
rather than a public issue. If you would rather not use GitHub for it, open a
normal issue saying only that you have something to report, and I will follow
up privately.

There is no bounty. I will acknowledge a report within a week and credit you in
the fix unless you would rather I did not.

## What this software is, so you can judge the risk

StreamPulse is an HTTP client and an HTTP server. It fetches URLs you configure
and it serves what it learned on a port you choose. It does not accept uploads,
does not write to any path except the incident state file and temporary files
during media inspection, and holds no credentials of its own beyond the
optional Slack webhook URL and any headers you configure it to send.

### Its HTTP servers are unauthenticated by design

Both the prober's port (metrics, the operator page, `/api/state`) and the
multiviewer's port are unauthenticated and unencrypted. That is a deliberate
choice for a component intended to sit behind something — the same posture as
a Prometheus exporter — and it means:

**Do not expose either port to the internet.** Bind them to a private
interface, or put them behind a reverse proxy that terminates TLS and
authenticates. `metrics_addr` in the config and `-addr` on the multiviewer
both take an interface, so `127.0.0.1:9090` is available if you only want
local access.

What an attacker who reaches those ports gets: your target list and their
URLs, including any query string in them; the health of your streams; and,
where media inspection is on, thumbnails of your content. The operator page and
`/api/state` are read-only — there is no endpoint that changes configuration,
adds a target, or stops a probe.

### It fetches what you tell it to

Targets come from the config file, and the prober will request them. It follows
redirects using Go's default client policy. It does not fetch URLs discovered
anywhere except inside the manifests of targets you configured — segment,
initialisation, and key URIs resolved against the manifest's own base.

If the config file is writable by someone you do not trust, they can make the
prober issue requests to anywhere it can reach, with any headers they specify.
Treat write access to the config as equivalent to the prober's network
position. The multiviewer takes its target list from the prober's `/api/state`,
so the same applies transitively: it will run `ffmpeg` against whatever URLs
that endpoint reports.

### Resource bounds

The places where a hostile or broken server could otherwise cost you:

- Manifest and key fetches are size-limited, and media inspection downloads cap
  at 16MB per object.
- `/api/state` responses read by the multiviewer cap at 8MB.
- Every external process (`ffprobe`, `ffmpeg`) runs under a context timeout
  with `WaitDelay` set, so one that ignores termination has its pipes closed
  rather than leaking the process.
- Segment sampling is capped per poll by `segment_sample`.

A target pointed at an endless response will be cut off by these limits rather
than exhausting memory. A target pointed at something enormous but finite — a
150MB initialisation segment, which happened during development — will be
truncated at the inspection cap.

### Containers

The published images run as a non-root user. The scratch image contains the
statically linked binary and nothing else: no shell, no package manager, no
writable filesystem beyond what you mount. The ffmpeg image is Debian-based and
therefore has a normal userland in it, which is a larger surface; use the
scratch image unless you need media inspection.

## Dependencies

There are none. `go.mod` has no `require` block, so the only third-party code
in a StreamPulse binary is the Go standard library. Media inspection execs
`ffprobe` and `ffmpeg` if they are present; nothing from FFmpeg is linked into
the binary, and the feature is off unless configured.

This means there is no dependency CVE surface to patch. It also means bugs in
the HLS and DASH parsers are mine rather than upstream's, and those parsers
handle untrusted input from whatever your targets' origins return. They are the
part of this codebase most worth looking at if you are looking for something.

## Supported versions

The most recent release. This is an MVP-stage project with one maintainer; I
will not be backporting fixes to older tags.
