# Decisions

The choices in this project that a competent engineer could reasonably have
made differently, with the reasoning, what each one costs, and what would change
it. Four of them were made twice, because the first answer was wrong and running
the thing against reality said so — those are marked and are the more useful
half of this document.

---

## 1. Probe actively; do not analyse logs

**Decision.** Fetch manifests and segments on a schedule from outside, the way a
player would, rather than ingesting CDN logs or player QoE beacons.

**Why.** Passive telemetry tells you that viewers have already suffered. By the
time a QoE dashboard dips, the people it describes have switched off. It is also
indirect: a spike in rebuffering could be the encoder, the packager, a CDN
region or a bad player release, and the beacon cannot distinguish them. An
active probe knows exactly what it asked for and exactly what came back.

**What it costs.** You only ever see what you probe. A synthetic prober watching
five channels knows nothing about the sixth, nothing about the real distribution
of devices and networks, and nothing about a fault that only manifests on a 2019
Samsung TV. It is a smoke detector, not a census.

**What would change it.** Nothing — these are complements, not alternatives. The
right end state is both, with the prober answering "is it broken and whose fault
is it" and the beacons answering "how many people does it affect". This project
is the half that was missing.

---

## 2. Standard library only

**Decision.** Write the HLS parser, the DASH parser, the Prometheus exposition,
the web UI and the notifiers from scratch. `go.mod` has no `require` block.

**Why.** Three reasons, in order of how much they mattered. It compiles anywhere
in seconds and has no dependency CVEs to chase, which for a tool whose job is to
be running when everything else is broken is worth more than convenience. It is
auditable end to end — someone can read all of it. And writing a manifest parser
is how you learn what a manifest actually promises; several checks exist only
because implementing the parser made an assumption visible.

**What it costs.** The parsers are the riskiest code in the repository and they
are mine. `grafov/m3u8` has had far more eyes on it. Full RFC 8216bis coverage is
not there and will not be without real work, and the DASH parser handles the four
addressing modes that exist in practice rather than everything the spec permits.

**What would change it.** A team that needs complete spec coverage and does not
want to own a parser. The README names the swap points deliberately: the parser
→ `grafov/m3u8`, metrics → `prometheus/client_golang`. None of them changes the
interfaces the prober uses, which was a design constraint rather than a
coincidence.

---

## 3. ffprobe and ffmpeg are shelled out, optional, and off by default

**Decision.** Media inspection execs external binaries rather than linking
bindings, is disabled unless configured, and lives in a separate container image.

**Why.** Linking cgo bindings to FFmpeg pulls its licensing into the binary and
its build complexity into the project, and would make a 10MB static binary a
150MB one for a feature most deployments do not need. Shelling out keeps the
dependency at arm's length: absent, the prober logs `media inspection disabled:
ffprobe not found on $PATH` at startup and runs every other check unchanged.

**What it costs.** Process spawn per inspection, and the failure modes of
`os/exec` — which bit twice. A missing `WaitDelay` left processes holding pipes
open on Linux in a way macOS never reproduced, and `ffprobe` given a URL rather
than a local file downloaded 150MB before anyone noticed.

**What would change it.** Needing per-frame analysis at scale, where the spawn
cost would dominate. Not likely here: this is a sampler, not a transcoder.

---

## 4. Incidents, not findings ✳ *revised*

**Decision.** Deduplicate repeated findings into incidents with a first-seen time
and a count, and notify only on transitions — firing and resolved.

**Why.** A frozen playlist polled every two seconds produces 1,800 identical
findings an hour. Nobody reads the 900th. An incident is the unit an operator
actually cares about: this thing is broken, since this time, still.

**What it costs.** A state machine, persistence across restarts so a redeploy
does not re-page everyone, expiry so a fault that stops being observed clears,
and a dedupe key that has to be exactly right. Getting the key wrong produced
log spam every sweep, because it included an error string containing a random
temp filename, so every poll looked like a new incident.

**✳ Revised.** The first version keyed dedupe on the message text. It had to
become a structured key plus a `saveFailing` flag. The downstream consequence
surfaced much later: because the prober notifies only on transitions, the
multiviewer's suggested "hold window" decay model — where a tile goes green if
no finding refreshes it — would have turned every tile green ninety seconds into
an ongoing outage. Health there is now replaced wholesale each poll instead.

---

## 5. Attribute cache faults as a split, not a verdict ✳ *revised*

**Decision.** When a manifest is stale, report how much of the lag is cache age
and how much the packager was already behind, rather than deciding which one is
at fault.

```
"live edge is 306s behind wall-clock [cache: Age 300s, HIT
 — the packager was 6s behind when this was generated; the rest is cache age]"
```

**Why.** A dead packager and a stale CDN edge produce identical bytes from
outside. The `Age` header is the only thing that separates them, and it separates
them continuously rather than categorically.

**✳ Revised.** The first version compared `Age` to the lag and picked a winner.
Against a real stale edge it said "fresher than the freeze, so the origin is
serving it" on a 300s-old manifest behind a 306s lag — pointing at the packager
when 98% of the problem was cache. A threshold was the wrong shape for the
question. The split is both more useful and harder to get wrong.

**What it costs.** A longer message, and it is only as good as the `Age` header.
Some CDNs do not send one; the code says so explicitly rather than guessing.

---

## 6. Derive thresholds from the manifest, not from configuration ✳ *revised*

**Decision.** Where the stream declares a number, check against that number.
`@timeShiftBufferDepth` for DVR depth, `@maxSegmentDuration` for segment length,
`EXT-X-TARGETDURATION` likewise, and segment duration as the unit for freeze and
staleness thresholds.

**Why.** Every configurable threshold is a threshold someone will set wrong, or
not set at all, and a check nobody configured is a check that does not run. A
number the packager put in its own manifest needs no configuration and is
correct per stream by construction.

**✳ Revised.** This was learned the hard way. Staleness thresholds were
originally absolute seconds; against a stream with 1.92-second segments they were
unreachable and the check never fired. They became fractions of a segment.
Separately, `edge_stale` false-positived on a perfectly healthy Unified stream
with a 6-second lag, because a threshold of a few segment durations is a few
seconds when the segments are 1.92s long. It gained a 30-second floor, and
respects `@suggestedPresentationDelay` when the manifest states one: a stream
telling players to sit well back from the edge is telling us it runs with
latency.

**What it costs.** Nothing that matters, but it does mean a check silently does
not run when the manifest omits the attribute it needs — `@maxSegmentDuration` is
optional. Absence is treated as "no ceiling declared" rather than as a fault.

---

## 7. Do not implement freeze detection for number-addressed DASH

**Decision.** `playlist_stalled` runs on timeline-addressed representations only.
On `$Number$` addressing it is deliberately not attempted.

**Why.** A number-addressed MPD is a formula, not a statement. It does not say
where the live edge is; the segment list is computed from the local clock. Comparing
that across polls compares our own clock to itself — it can only ever advance,
so the check would never fire. That is worse than not checking, because it would
appear in the check list and look like coverage.

Those streams are covered elsewhere: the segment sample requests the segment at
the computed edge every poll, and a packager that has stopped fails that fetch.
`segment_availability` is the freeze check for them.

**What it costs.** An asymmetry that has to be explained, and a slightly slower
detection path for number-addressed streams, since it takes a failed fetch rather
than an unchanged manifest.

**What would change it.** Nothing. This is the correct answer; the temptation is
to add the check anyway so the table looks complete.

---

## 8. Cross-poll state lives in the process, keyed by target ✳ *revised*

**Decision.** Freeze detection, rollback detection, PDT progression and key
rotation keep their previous reading in memory, keyed by target name plus
playlist or representation.

**Why.** These checks need to compare this poll against the last one. Redis or a
database would make the prober depend on infrastructure that can itself be down,
which for a monitoring tool is backwards. In-process state means the prober is
one binary with no runtime dependencies.

**What it costs.** State is lost on restart, so the first poll after a redeploy
establishes a baseline instead of detecting anything — a freeze spanning a
restart is missed. Horizontal scaling means each instance keeps its own state,
which is fine because each is an independent vantage, but it does mean two
instances cannot share the work of one target list.

**✳ Revised.** It was keyed by URL. A 28-hour soak found what that does when two
targets watch the same stream: they share one entry, and the slower one reports
the faster one's progress as a rollback. The worse half never fired in that run
— a frozen edge on one target is reported as a rollback and never as a stall.
Now keyed by target name first. The full story is in the
[README](../README.md#what-running-it-for-a-day-found).

---

## 9. Poll the config file; do not watch it

**Decision.** Config hot-reload reads the file every ten seconds and compares a
SHA-256 of the contents, rather than using filesystem notifications.

**Why.** It needs no dependency, and — the real reason — it is indifferent to
*how* the file changed. An editor renaming a temp file over the original, a
Kubernetes ConfigMap remounted as a new symlink, and a plain in-place write all
look identical to a content hash. They look very different to a naive inode
watch, and the ConfigMap case is the one that matters in production.

**What it costs.** Up to ten seconds of latency, which nobody notices, and a
`stat` plus a read every ten seconds, which nothing notices.

**✳ Amended.** A file caught mid-write parsed as a syntax error and was reported
as one. A version that fails to load now gets one more tick to settle and is
reported only if the same bytes still fail. A real typo is reported one poll
later than before; a half-written file is not reported at all.

---

## 10. The multiviewer is a separate binary

**Decision.** `cmd/mosaic` is its own program and its own image, not a package
inside the prober.

**Why.** The resource profiles are not variations on each other. A prober samples
one segment per poll and must stay light enough to be trusted when everything
else is on fire; a multiviewer decodes every channel continuously, forever. In
one process an ffmpeg storm could starve the alerting path — and it would storm
precisely when streams go bad, which is when the grabbers thrash and respawn. It
would also force ffmpeg into an image that is 10MB of scratch container
specifically because it needs none.

**What it costs.** Two containers to deploy instead of one, and the wall goes
blank if the prober is unreachable — though a multiviewer that cannot reach its
prober arguably *should* look broken.

**What would change it.** A hard requirement for a single `docker run` demo at a
handful of targets. That is a real argument and it was close.

---

## 11. The wall reads the prober's API; it is not wired into it

**Decision.** The multiviewer polls `/api/state` for the target list, stream
URLs, reachability and open incidents. It has no config file, no plugin
interface, and no notifier hook.

**Why.** The alternative — the one the original package proposed — was a notifier
adapter pushing findings into a health store inside the wall. That store is a
second copy of what "critical" means, and two records of one observation drift;
the one on the dashboard is the one nobody notices is wrong. Reading the prober's
own answer makes drift structurally impossible, and it turned out to be a
*smaller* integration than the push model, not a bigger one: no adapter, no
severity mapping, no expiry rule.

**What it costs.** A poll every two seconds, and a hard dependency on an endpoint
that is now a public contract rather than an implementation detail.

---

## 12. MJPEG and server-sent events, not WebRTC or player-based tiles

**Decision.** Each tile is motion-JPEG in a plain `<img>`; status arrives
separately over SSE.

**Why.** Zero client-side media decoding, no JavaScript build step, no player
library, and flat cost however many tiles are on the wall — the browser is
decoding JPEGs, not running twenty HLS players. Status on a separate channel
means a tile's border and numbers keep updating even when its picture is stuck,
which is exactly the state you most need to see.

**What it costs.** A known limit: each tile is a long-lived HTTP response, and
browsers allow about six connections per origin over HTTP/1.1. A wall past six
tiles wants HTTP/2, which means TLS. This is documented rather than fixed,
because the fix — compositing one mosaic stream server-side — is real work and
nobody is yet running more than six channels.

---

## 13. Pace VOD input, not live ✳ *revised*

**Decision.** The grabber passes `-re` and `-stream_loop -1` to ffmpeg for
non-live inputs only, using the liveness the prober already reports.

**✳ Revised.** There was no such distinction at first, and it was visible within
an hour of running the demo stack: ffmpeg reads a finite input as fast as it can
download it, so a ten-minute VOD clip went by in seventy seconds at eight and a
half times speed, hit the end, exited, and was restarted from the beginning by
the backoff path meant for failures.

**Why not apply it to everything.** A live stream is already paced by segment
availability. `-re` on top of that is at best redundant and at worst a slow drift
behind the live edge.

**What it costs.** One more thing the wall takes from `/api/state`, and a target
whose liveness the prober has not yet decided is treated as live — the safer
guess, since pacing a live stream is harmless and not pacing a VOD one is the
bug.

---

## 14. Mutation testing as the standard of evidence

**Decision.** For each new check, break the code deliberately and confirm a test
fails. A test that passes against broken code is not evidence.

**Why.** It repeatedly caught vacuous tests here — tests that exercised a code
path and asserted something that was true either way. The discipline also
produces better tests, because the question "what mutation would this catch"
is sharper than "does this pass".

**What it costs.** Roughly a third more time per check, and it is done by hand:
a `sed`, a run, a revert. There is no `go-mutesting` in the loop.

**What it does not buy.** Everything in this document marked ✳ was found by
running the thing, not by testing it. A 28-hour soak and one afternoon of
actually starting the demo stack found four defects between them that ~10,000
lines of mutation-tested tests did not. The tests were reasoning about shapes
that had already been imagined.

---

## 15. Vantage as a constant label, not a separate metric

**Decision.** When several probers watch the same channels from different
regions, the region is attached as a label to every series and every incident,
set once at startup.

**Why.** Without it the second prober's `streampulse_probe_up{target="x"}`
silently overwrites the first's in Prometheus, and you have two probers producing
one answer. As a constant label it costs nothing to apply and cannot be forgotten
at a call site — which matters, because there are about fifty call sites and
forgetting it at one would be invisible.

**What it costs.** Cardinality multiplies by the number of vantages, which is
the honest price of the information. The label is omitted entirely when unset,
so a single-prober deployment does not get a label it never asked for and
dashboards built without one keep working.

---

## What I would do differently

Three things, if starting again:

**Run it for a day before writing the second half of the checks.** Four of the
revisions above would have been avoided by having the feedback earlier. The
pattern is consistent enough to be a rule: a check written against a fixture and
a check written against a live stream are different checks, and only one of them
has met reality.

**Write the parser tests against real manifests sooner.** The synthetic fixtures
were minimal — they omitted `@codecs` because nothing needed it — which meant a
structural check found "faults" in eight of my own test manifests before it found
any in a real one.

**Decide the unit of state before writing the first cross-poll check.** Keying by
URL was never a decision; it was the obvious thing at the time, and it survived
until a soak found it. The things that bite are rarely the decisions you
agonised over.
