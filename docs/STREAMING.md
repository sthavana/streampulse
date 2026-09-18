# How a stream gets to a viewer, and where it breaks

Background for the checks StreamPulse runs. It assumes you know what a video
file is and nothing else, and it ends where the [check
tables](../README.md#what-it-checks-today-hls) begin: every section closes with
what goes wrong at that stage and which check catches it.

The through-line, if you only read one paragraph: **a stream is a contract
written in a manifest and honoured by four separate systems.** The encoder
promises rungs that can be switched between. The packager promises segments
that exist at the URLs it advertises. The origin promises to keep producing
them. The CDN promises to hand over what the origin actually said. Every
outage in this document is one of those four breaking a promise while the
other three keep theirs, which is why "the stream is down" is never a useful
sentence on its own.

**Contents**

1. [The chain, end to end](#1-the-chain-end-to-end)
2. [Encoding and the ABR ladder](#2-encoding-and-the-abr-ladder)
3. [Packaging](#3-packaging)
4. [HLS](#4-hls)
5. [DASH](#5-dash)
6. [Low latency](#6-low-latency)
7. [Origin and CDN](#7-origin-and-cdn)
8. [Ad insertion](#8-ad-insertion)
9. [Where it breaks: a summary](#9-where-it-breaks-a-summary)

---

## 1. The chain, end to end

```
  camera / file            encoder            packager           origin
  ┌──────────┐      ┌──────────────┐    ┌──────────────┐   ┌──────────┐
  │ mezzanine│─────▶│ transcode to │───▶│ segment +    │──▶│ serve    │
  │  SDI/RTP │      │ a ladder of  │    │ write        │   │ manifest │
  │  / mp4   │      │ renditions   │    │ manifests    │   │ + media  │
  └──────────┘      └──────────────┘    └──────────────┘   └────┬─────┘
                       H.264/HEVC/AV1     TS or CMAF             │
                       AAC/AC-3           HLS and/or DASH        │
                                                                 ▼
                                                        ┌─────────────┐
   ┌────────┐         ┌──────────────┐                  │ origin      │
   │ player │◀────────│  CDN edge    │◀─────────────────│ shield      │
   └────────┘         └──────────────┘                  └─────────────┘
    picks a rung       caches segments long,
    per segment        manifests briefly
```

Four handoffs, four places to lie. The player only ever sees the last one, so
from a player's vantage a dead encoder, a stuck packager, a failed origin and a
stale CDN edge all look the same: a manifest that stops changing.

Distinguishing them from outside is the central problem this tool exists for.

---

## 2. Encoding and the ABR ladder

### Why a ladder exists

A viewer on a train and a viewer on fibre cannot be served the same file. Adaptive
bitrate streaming solves this by encoding the same content several times at
different bitrates and resolutions — **renditions**, or **rungs** — and letting
the player switch between them, segment by segment, as its bandwidth estimate
changes.

A typical live ladder:

| Rung | Resolution | Video bitrate | Profile |
|---|---|---|---|
| 1 | 1920×1080 | 5,000 kbps | High |
| 2 | 1280×720 | 3,000 kbps | Main |
| 3 | 960×540 | 1,600 kbps | Main |
| 4 | 640×360 | 800 kbps | Main |
| 5 | 480×270 | 400 kbps | Baseline |

Plus audio, usually once per language rather than once per video rung — audio
is small and does not need to adapt as aggressively.

Ladder design is its own discipline. The rungs should be spaced so each step
is a meaningful bandwidth saving (a rule of thumb is a ratio of about 1.5–2×
between neighbours); too many rungs wastes encoding cost and confuses ABR
logic, too few makes switching jarring. **Per-title encoding** goes further and
picks a ladder per piece of content, since an animated cartoon and a football
match do not need the same bitrates for the same quality.

### Transcoding vs transmuxing

Two words that get used interchangeably and should not be:

- **Transcoding** decodes and re-encodes. The pixels are recompressed. It is
  expensive, lossy, and what produces the ladder.
- **Transmuxing** (or repackaging) rewrites the container without touching the
  compressed video. Turning an MPEG-TS segment into an fMP4 segment is a
  transmux: same H.264 bitstream, different box structure. It is cheap and
  lossless.

Most pipelines transcode once into a mezzanine ladder, then transmux into as
many delivery formats as they need.

### The thing that makes switching possible

A player switching from rung 2 to rung 3 mid-stream is splicing two different
encodes together. For that to work without a visible glitch, every rung must be
**IDR-aligned**: each segment must start with an IDR frame (a keyframe that
resets all prediction), at *exactly* the same presentation timestamp in every
rung.

This is the single most important constraint in ABR encoding, and it drives
several others:

- **Closed GOPs.** A GOP (group of pictures) that references frames outside
  itself cannot be decoded standalone. Segments must be independently decodable,
  so GOPs must be closed at segment boundaries.
- **Fixed GOP length**, or at least a GOP length that divides the segment
  duration. A 2-second segment at 50fps with a 100-frame GOP works; a
  scene-cut-driven adaptive GOP does not, unless the encoder is told to force
  an IDR at segment cadence.
- **Identical timebase and PTS** across rungs. All rungs encode the same source
  timeline; if one drifts, switching produces a jump or a freeze.
- **`segmentAlignment="true"`** in DASH, which is the packager asserting exactly
  this property.

If alignment is wrong the stream usually still *plays*, on most players, most of
the time — which is why it survives testing and reaches production. What you get
is occasional frozen frames or audio pops at the moment a player switches rungs,
on some devices, reported by a fraction of viewers.

### Codecs

| | Video | Audio |
|---|---|---|
| Universal | H.264 / AVC | AAC-LC |
| Efficient | HEVC / H.265 (~40% better, licensing complexity) | HE-AAC, xHE-AAC |
| Newer | AV1 (royalty-free, encoder cost high) | Opus |
| Broadcast | — | AC-3, E-AC-3 (Dolby Digital) |

The `CODECS` attribute in HLS and `@codecs` in DASH carry an RFC 6381 string —
`avc1.640028`, `mp4a.40.2`, `hvc1.1.6.L93.B0` — which tells a player whether it
can decode a rung *before fetching any of it*. Getting it wrong, or omitting it,
means the player either fetches something it cannot play or skips something it
could.

### What goes wrong here

| Symptom | What is actually wrong |
|---|---|
| Glitch on rung switch | IDR misalignment between renditions |
| Rung plays on desktop, not on TV | `CODECS` string wrong, or profile/level above the device's capability |
| One rung is black or absent | Encoder instance died; the manifest still advertises it |
| Declared 1080p, delivered 720p | Ladder misconfiguration, or a rung mapped to the wrong output |

**StreamPulse:** `resolution_mismatch` and `codec_mismatch` compare what the
manifest declares against what ffprobe finds in the actual media;
`audio_track_missing` / `video_track_missing` catch a declared track that is not
in the bytes; `black_frames` and `frozen_video` catch a rung that is technically
delivering and showing nothing. These need the [optional
inspection](../README.md#looking-inside-the-media-optional) build.

---

## 3. Packaging

The packager takes encoded elementary streams and produces (a) media segments
and (b) manifests describing them.

### Containers: TS and CMAF

**MPEG-TS** is the older option, inherited from broadcast. A 188-byte packet
structure with PIDs, PAT/PMT tables, continuity counters and PCR timestamps.
Robust over lossy transports, wasteful over HTTP (about 10–15% overhead), and
still the only thing some legacy devices accept.

**fMP4 / CMAF** is the modern one. ISO Base Media File Format, fragmented: an
initialisation segment carrying `ftyp` and `moov` (the codec configuration and
track metadata), then media segments each carrying `moof` + `mdat` pairs. Lower
overhead, required for HEVC in HLS, and — the important part — **the same
segments can serve both HLS and DASH**.

That convergence is what CMAF is for. Before it, serving both protocols meant
packaging everything twice and storing it twice. With CMAF you write one set of
segments and two manifests over them:

```
        /video_1080p/init.mp4
        /video_1080p/1.m4s, 2.m4s, 3.m4s …
             ▲                        ▲
             │                        │
      master.m3u8              manifest.mpd
      (HLS view)               (DASH view)
```

An **initialisation segment** is required to decode anything: it carries the SPS
and PPS for H.264, the decoder configuration, the timescale. Fetch a media
segment without it and you have bytes you cannot decode. It is referenced by
`EXT-X-MAP` in HLS and `<Initialization>` in DASH, and it is small, cacheable
forever, and catastrophic when missing — playback cannot start at all, on any
rung.

### Segment duration

The defining trade-off of the whole system:

| Duration | Latency | Overhead | Playlist size | CDN efficiency |
|---|---|---|---|---|
| 1–2 s | Low | High (more requests, more IDRs) | Large | Worse |
| 4 s | Balanced | Balanced | Balanced | Good |
| 6–10 s | High | Low | Small | Best |

Latency is roughly *segment duration × player buffer depth*, and players
typically buffer three segments before starting. Six-second segments therefore
mean roughly 18–30 seconds behind live before anything else goes wrong. Two-second
segments get you to 6–10 seconds at the cost of three times the request rate.
Going below that requires the [low-latency](#6-low-latency) machinery, which is
a different mechanism rather than more of the same.

More IDRs also costs quality: keyframes are expensive, so a 1-second segment at
a fixed bitrate has measurably worse picture than a 6-second one.

### Static vs just-in-time packaging

- **Static**: segments written to storage ahead of time. Cheap to serve, storage
  cost multiplies by the number of formats, and changing anything means
  repackaging.
- **Just-in-time (JIT)**: the origin holds one mezzanine format and packages on
  request, usually with its own cache. One copy stored, more CPU per request,
  and a cache miss storm can take the origin down.

JIT is why some origins are slow on first request and fast thereafter, and why
`segment_availability` failures sometimes correlate with cache misses rather
than with anything being genuinely absent.

### DRM

Encryption happens at packaging time. Two layers to keep separate:

- **The encryption scheme** — how the bytes are encrypted. `cenc` (AES-CTR) and
  `cbcs` (AES-CBC with pattern encryption) are the two CENC schemes. They are
  not interchangeable: FairPlay requires `cbcs`, which is why `cbcs` has become
  the de facto choice for CMAF.
- **The DRM system** — who issues the key. Widevine (Google/Android/Chrome),
  PlayReady (Microsoft/Xbox/Smart TVs), FairPlay (Apple). Serving all three from
  one set of segments is **multi-DRM**, and works because the segments are
  encrypted once under one key, with each system given its own way to deliver
  that key.

The manifest carries the initialisation data a player needs to *request* a
licence, not the key itself:

- **HLS**: `EXT-X-KEY` with `METHOD`, `URI`, `KEYFORMAT`. For AES-128 the URI is
  a fetchable key. For FairPlay it is a `skd://` URI handed to the platform.
- **DASH**: `<ContentProtection>` elements — one for the scheme
  (`urn:mpeg:dash:mp4protection:2011` carrying `@cenc:default_KID`) and one per
  DRM system, each with a `<cenc:pssh>` box.

**Key rotation** changes the content key periodically so a leaked key is worth
only a window rather than the whole stream. It is the security feature most
likely to fail silently: everything keeps playing when rotation stops, because
the old key still works.

### What goes wrong here

| Symptom | What is actually wrong |
|---|---|
| Nothing plays, any rung | Init segment missing or 404 |
| Plays on Apple, not elsewhere (or vice versa) | Wrong encryption scheme for the DRM system |
| Licence requests fail | Malformed PSSH, or `default_KID` not matching the media |
| Stream plays unencrypted | Packager lost its DRM config; clear segments on a protected stream |
| Rebuffering on a healthy CDN | A segment longer than the declared maximum; buffers sized wrong |

**StreamPulse:** `init_segment_availability`, `map_missing_uri`,
`unexpected_clear_segments`, `pssh_malformed`, `pssh_empty`, `kid_invalid`,
`key_fetch`, `key_size_invalid`, `key_rotation_stalled`,
`segment_duration_violation`, `targetduration_violation`.

---

## 4. HLS

Apple's format, originally RFC 8216, now maintained as the HLS specification
with regular revisions. Two levels of playlist, both plain text, both UTF-8,
both beginning `#EXTM3U`.

### The master playlist

Lists the rungs and the alternative tracks. It is fetched once (or occasionally
refetched) and contains no media.

```m3u8
#EXTM3U
#EXT-X-VERSION:7

#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="English",LANGUAGE="en",
             DEFAULT=YES,AUTOSELECT=YES,URI="audio/en/index.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="German",LANGUAGE="de",
             DEFAULT=NO,AUTOSELECT=YES,URI="audio/de/index.m3u8"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",
             DEFAULT=NO,URI="subs/en/index.m3u8"

#EXT-X-STREAM-INF:BANDWIDTH=5200000,AVERAGE-BANDWIDTH=4800000,
                  RESOLUTION=1920x1080,CODECS="avc1.640028,mp4a.40.2",
                  FRAME-RATE=50.000,AUDIO="aac",SUBTITLES="subs"
video/1080p/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3200000,RESOLUTION=1280x720,
                  CODECS="avc1.4d401f,mp4a.40.2",AUDIO="aac",SUBTITLES="subs"
video/720p/index.m3u8
```

Points worth knowing:

- **`BANDWIDTH` is the peak**, not the average — it must be an upper bound on
  the rung's bitrate over any segment, because the player uses it to decide
  whether the rung fits the connection. `AVERAGE-BANDWIDTH` is optional and
  advisory.
- **`AUDIO="aac"` is a reference, not a definition.** It points at a `GROUP-ID`
  that `EXT-X-MEDIA` lines must declare. A variant referencing a group that does
  not exist is a dangling pointer: the player has video and no sound.
- **Demuxed vs muxed.** If audio is in a separate `EXT-X-MEDIA` rendition, the
  video playlists contain video only, even though their `CODECS` string usually
  still lists the audio codec — because `CODECS` describes what the *presentation*
  needs, not what that one playlist contains. This trips up naive validators
  constantly.
- **`DEFAULT=YES` must appear at most once per group.** Two defaults means the
  player picks arbitrarily, and different players pick differently.

### The media playlist

One per rung, and the thing that changes as a live stream progresses.

```m3u8
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:1547
#EXT-X-PROGRAM-DATE-TIME:2026-09-18T10:14:32.000Z
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.000,
seg1547.m4s
#EXTINF:4.000,
seg1548.m4s
#EXTINF:4.000,
seg1549.m4s
```

| Tag | Means |
|---|---|
| `EXT-X-TARGETDURATION` | The maximum segment duration, rounded to an integer. Mandatory. Players use it to decide how often to reload. **No segment may exceed it.** |
| `EXT-X-MEDIA-SEQUENCE` | The sequence number of the first segment listed. Increments as the window slides. |
| `EXTINF` | This segment's exact duration, fractional. |
| `EXT-X-MAP` | The initialisation segment. Required for fMP4. |
| `EXT-X-PROGRAM-DATE-TIME` | Wall-clock time of the next segment's first sample. Optional but the only way to relate media time to real time. |
| `EXT-X-ENDLIST` | The playlist is complete — this is VOD, or a live stream that has ended. Its presence means the player stops reloading. |
| `EXT-X-DISCONTINUITY` | Timeline, codec or timestamp discontinuity follows. Ad breaks, source switches. |
| `EXT-X-KEY` | Encryption parameters for subsequent segments. |

**How live works.** There is no "live" flag. A playlist is live if it lacks
`EXT-X-ENDLIST`. The player reloads it every target duration; each reload the
packager has appended a segment and, once the window is full, dropped the oldest
and incremented `EXT-X-MEDIA-SEQUENCE`. The window is typically 3–10 segments for
low latency or hours for a DVR.

**The three ways to tell it has stopped:**

1. `EXT-X-MEDIA-SEQUENCE` stops incrementing — the window is not sliding.
2. The playlist is byte-identical poll after poll.
3. `PROGRAM-DATE-TIME` projected to the end of the window falls behind wall clock.

The third is the most sensitive and the only one that catches an encoder that is
falling behind while still publishing. It is also the only one that needs the
packager to have emitted PDT at all.

### What goes wrong here

| Symptom | What is actually wrong |
|---|---|
| Stream frozen on the last frame | Media sequence not advancing; packager stopped |
| Playback jumps backwards | Sequence went backwards — an origin failover to a lagging instance |
| Player stops after one segment | `EXT-X-ENDLIST` on a live stream |
| Video, no audio | `AUDIO` group referenced but no `EXT-X-MEDIA` declares it |
| Rebuffer on a healthy connection | `EXTINF` exceeding `EXT-X-TARGETDURATION` |
| Stream drifts further behind live all day | Encoder slower than real time; PDT falls behind wall clock |

**StreamPulse:** `playlist_stalled`, `playlist_rollback`, `unexpected_endlist`,
`rendition_group_missing`, `rendition_duplicate_name`,
`rendition_multiple_default`, `closed_captions_with_uri`,
`targetduration_violation`, `targetduration_missing`, `pdt_stale`,
`pdt_not_advancing`, `short_window`, `no_segments`, `segment_availability`.

---

## 5. DASH

ISO/IEC 23009-1. One XML document — the MPD, Media Presentation Description —
describing the entire presentation, rather than HLS's tree of playlists. That
single difference drives most of the others: a DASH player fetches one document
and knows everything, where an HLS player fetches a master playlist and then one
media playlist per rung it cares about.

### Structure

```
MPD  @type=dynamic|static  @availabilityStartTime  @timeShiftBufferDepth
 └── Period  @id @start @duration              (an ad break opens a new one)
      └── AdaptationSet  @contentType @lang    (one track: video, one language)
           └── Representation  @id @bandwidth  (one rung)
                └── SegmentTemplate / SegmentList / SegmentBase
```

- **Period** is a time span of the presentation. Most live streams have one,
  until an ad break splits them.
- **AdaptationSet** groups interchangeable Representations — the video ladder is
  one set, each audio language another.
- **Representation** is a rung, identified by `@id`, which is also what
  `$RepresentationID$` in a template expands to.

Attributes are **inherited downwards**: `@mimeType`, `@codecs`, `@width` and the
segment-addressing elements can be declared at any level and apply to everything
beneath. A parser that does not fold inheritance will see half the manifest as
empty.

### Segment addressing: the four ways

This is where DASH's complexity concentrates, and where most implementation bugs
live.

**1. SegmentTemplate with `$Number$`** — segments are numbered arithmetically.

```xml
<SegmentTemplate media="v/$RepresentationID$/$Number%05d$.m4s"
                 initialization="v/$RepresentationID$/init.mp4"
                 duration="96000" timescale="24000" startNumber="1"/>
```

Segment *n* covers presentation time `(n − startNumber) × duration / timescale`.
The manifest is a formula, not a list: it does not change as the stream
progresses, and the player computes which segment should exist right now from its
own clock. Cheap — the MPD can be cached for a long time — and it means **the
manifest cannot tell you the stream has stopped**, because the formula keeps
producing plausible URLs forever.

**2. SegmentTemplate with `$Time$` and a SegmentTimeline** — segments are listed.

```xml
<SegmentTemplate media="v/$RepresentationID$/$Time$.m4s" timescale="48000">
  <SegmentTimeline>
    <S t="85884561024000" d="96256" r="2"/>
    <S d="95232"/>
    <S d="96256" r="4"/>
  </SegmentTimeline>
</SegmentTemplate>
```

`@t` is the start time in timescale units, `@d` the duration, `@r` the number of
*additional* repeats (`r="2"` means three segments). `@r="-1"` means repeat until
the next `<S>` or the end of the period. Only the first `<S>` usually carries
`@t`; the rest continue from where the last one ended.

The timeline handles variable segment durations — unavoidable with ad breaks or
variable frame rates — and, unlike `$Number$`, **the manifest states where the
live edge is**, so a frozen packager produces a byte-identical timeline. That is
what makes freeze detection possible on timeline-addressed streams and impossible
on number-addressed ones.

A run whose `@t` does not continue where the previous run ended is a **hole in
the timeline**: media time that no segment covers. Players stall or skip there.

**3. SegmentList** — every URL enumerated explicitly. Verbose, rare, used for
short VOD.

**4. SegmentBase** — one file per Representation with an index (`sidx`) box, and
byte-range requests into it. Common for VOD; the init data is a byte range at the
front rather than a separate file.

### Live: the clock is the mechanism

A `@type="dynamic"` MPD has no sliding window in the HLS sense. Instead:

| Attribute | Means |
|---|---|
| `@availabilityStartTime` | Wall-clock time corresponding to presentation time zero. Everything is computed from this. |
| `@timeShiftBufferDepth` | How far back seeking is guaranteed to work — the DVR promise. |
| `@minimumUpdatePeriod` | How often a player should re-fetch the MPD. Zero means every segment. |
| `@publishTime` | When this version of the MPD was generated. |
| `@maxSegmentDuration` | Upper bound on any segment; players size buffers from it. |
| `@suggestedPresentationDelay` | How far behind live the packager suggests playing. |

The live edge is therefore `now − availabilityStartTime`, adjusted for the
period's start. A player computes which segments exist from its own clock, which
is why **clock skew is a first-class failure mode in DASH** and why `UTCTiming`
exists: it tells the player whose clock to trust.

### What goes wrong here

| Symptom | What is actually wrong |
|---|---|
| Stall at a fixed point every time | Hole in the SegmentTimeline |
| Stall exactly when an ad starts | Gap or overlap at a period boundary; SSAI arithmetic wrong |
| Seeking back fails despite a long DVR | Window shorter than `@timeShiftBufferDepth` claims |
| One rung serves another's media | Two Representations sharing an `@id` |
| Player has no audio to select | AdaptationSet declared with no Representations |
| Every viewer a different distance behind live | No `UTCTiming`; each player trusting its own clock |
| Nothing plays on some devices | `@codecs` or `@mimeType` missing, so capability cannot be determined |

**StreamPulse:** `timeline_gap`, `timeline_overlap`, `period_gap`,
`period_overlap`, `window_below_declared`, `representation_duplicate_id`,
`dependency_missing`, `adaptation_set_empty`, `period_empty`,
`adaptation_set_multiple_main`, `representation_missing_mime`,
`representation_missing_codecs`, `segment_duration_violation`,
`utc_timing_missing`, `unexpected_static`, `edge_stale`, `playlist_stalled`,
`playlist_rollback`.

---

## 6. Low latency

Standard HLS and DASH put a viewer 15–45 seconds behind live. Broadcast is
around 5. For sport and betting that gap is the product, so both formats grew a
low-latency mode — and both work the same way underneath, by **letting a player
fetch part of a segment before the segment is finished**.

### Where the latency actually is

```
encoder    packager    origin    CDN     player buffer
 1-2s        0.5s       0.1s     0.2s     3 × segment duration   ← the big one
```

The player buffer dominates, and it is a function of segment duration. You
cannot fix it by making segments shorter forever — at some point the request
overhead and the keyframe cost dominate. So instead you keep 4-second segments
and deliver them in pieces as they are produced.

### The enabling trick: chunked transfer

A CMAF segment is a sequence of **chunks**, each a `moof` + `mdat` pair, each
independently deliverable. The packager writes chunks as the encoder produces
them, and the origin responds to a request for the not-yet-finished segment with
HTTP **chunked transfer encoding** — sending each chunk as it lands, keeping the
response open.

The player therefore starts decoding a segment that is still being written.

This is also the one that fails invisibly. If any hop — the origin, a CDN edge,
a reverse proxy — buffers the whole response before forwarding it, everything
still works: the manifest is correct, every segment serves, players play. They
just play seconds behind where the design says. Nothing 404s, no check on
availability or freshness fires, and the entire low-latency investment is inert.

**The tell:** a response carrying a `Content-Length` for a segment that is still
being produced. A segment still being written cannot have a known length, so a
`Content-Length` on one proves something waited for the whole thing.

### LL-HLS

Apple's version adds **partial segments**:

```m3u8
#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,PART-HOLD-BACK=1.020,
                      CAN-SKIP-UNTIL=24.0
#EXT-X-PART-INF:PART-TARGET=0.340
...
#EXTINF:4.00000,
seg1547.m4s
#EXT-X-PART:DURATION=0.34000,URI="seg1548.0.m4s"
#EXT-X-PART:DURATION=0.34000,URI="seg1548.1.m4s"
#EXT-X-PART:DURATION=0.34000,URI="seg1548.2.m4s",INDEPENDENT=YES
#EXT-X-PRELOAD-HINT:TYPE=PART,URI="seg1548.3.m4s"
```

- **`EXT-X-PART`** lines expose the pieces of the in-progress segment.
- **`EXT-X-PRELOAD-HINT`** names the next part before it exists; the player
  requests it and the origin holds the response open until it does.
- **Blocking playlist reload** (`CAN-BLOCK-RELOAD=YES`): the player asks for the
  playlist *as of* a future part, and the origin holds the request rather than
  returning a stale copy. This removes polling latency entirely.
- **`PART-HOLD-BACK`** tells the player how close to the edge it may play —
  at least three part durations.
- **Delta playlists** (`CAN-SKIP-UNTIL`) let a player request only the changed
  tail, because a full DVR playlist re-sent every 340ms is a lot of bytes.

### LL-DASH

DASH keeps the same MPD and adds three attributes:

- **`@availabilityTimeOffset`** on the SegmentTemplate: how much *earlier* than
  its nominal completion a segment becomes fetchable. This is what says "start
  requesting this before it exists."
- **`@availabilityTimeComplete="false"`**: the segment is available before it is
  complete. Together these two are the declaration of chunked delivery.
- **`<ServiceDescription><Latency target="3000" min="2000" max="6000"/>`**: the
  latency the operator is aiming for, in milliseconds, plus a `<PlaybackRate>`
  range letting the player speed up slightly to catch up.

And `UTCTiming` becomes essential rather than advisory. At a three-second target
a few seconds of clock drift is the entire budget, and without a stated time
source every viewer is guessing from their own device clock.

### What goes wrong here

| Symptom | What is actually wrong |
|---|---|
| Latency is 15s on a 3s-target stream | Something is buffering whole segments; chunked delivery is not happening |
| Latency varies wildly per viewer | No `UTCTiming`; players using their own clocks |
| Players stall at the edge then recover | Playing too close to the edge; `PART-HOLD-BACK` too small or ignored |
| Works direct from origin, not via CDN | The CDN is not configured to pass through chunked responses |

**StreamPulse:** `chunked_delivery_missing` (the `Content-Length` tell, run
against a segment that is genuinely still in production), `utc_timing_missing`,
and `edge_stale` measured from segment completion rather than availability — so
a low-latency stream is not flattered by its own `@availabilityTimeOffset`.

---

## 7. Origin and CDN

### What sits where

- **Origin** serves manifests and segments. Either a packager producing them
  just-in-time, or storage holding pre-packaged output.
- **Origin shield / mid-tier**: a single caching layer in front of the origin so
  that a hundred edge nodes missing simultaneously produce one origin request,
  not a hundred. The thing that stops a popular stream from killing its own
  origin.
- **Edge**: the node the viewer actually talks to. Hundreds of them, each with
  an independent cache.

The critical consequence: **there is no single "the CDN".** Two requests seconds
apart can land on different edges with different cache states, which is why a
stream can look fine from one place and broken from another, and why the same
probe can see an edge one segment behind the one it saw a moment ago.

### Caching policy

Manifests and segments want opposite treatment:

| | TTL | Why |
|---|---|---|
| Live manifest | 1–2 s, or the segment duration | It changes constantly; a stale one freezes the stream |
| Segment | Hours to forever | Immutable once written; identified by a unique URL |
| Init segment | Forever | Never changes |
| VOD manifest | Minutes to hours | Never changes after publication |

Getting the manifest TTL wrong is the classic CDN misconfiguration: cache a live
manifest for 60 seconds and every viewer sees a stream that advances in
60-second jumps, while the origin is perfectly healthy.

### The headers that matter

| Header | What it tells you |
|---|---|
| `Age` | Seconds since the origin generated this response, as the caches in between report it. **The reliable one.** |
| `Cache-Control` | `max-age` for browsers, `s-maxage` for shared caches; `stale-while-revalidate` allows serving stale while refreshing |
| `X-Cache`, `CF-Cache-Status`, `X-Cache-Status` | HIT / MISS / STALE — vendor-specific spelling, and there are many |
| `Last-Modified` / `ETag` | Enables conditional requests |

A CDN chain reports its layers in order, shield first: `X-Cache: HIT, MISS`
means the shield had it and the edge did not — or the other way round, depending
on the vendor. **The element nearest the client is the one that served you.**

### Why this is the hardest fault to attribute

A frozen manifest can mean:

1. The encoder died.
2. The packager died or lost its input.
3. The origin is failing and a load balancer is serving an old copy.
4. A CDN edge has a stale object and is not revalidating.

From outside, all four produce identical bytes. And the fourth is *still a real
viewer-facing outage* — everyone hitting that edge sees a frozen stream — so
"bypass the cache and check the origin" answers the wrong question. You need to
know which, because they are different teams and different fixes.

`Age` is what separates them. If the response is younger than the freeze, the
origin generated this frozen manifest just now, and the problem is upstream of
the CDN. If `Age` is at least as old as the freeze, one stale cached object
accounts for everything observed and the origin may be perfectly healthy behind
it.

Better still, treat it as a split rather than a verdict: of an observed lag of
*L*, exactly `Age` seconds is cache and the remaining `L − Age` is how far behind
the packager already was when it generated that manifest.

```
"live edge is 306s behind wall-clock [cache: Age 300s, HIT
 — the packager was 6s behind when this was generated; the rest is cache age]"
```

A threshold comparing `Age` to the lag would call that one "fresher than the
lag" and blame the packager, when 98% of it is cache. This is not hypothetical:
it is what the first version of this check did.

### Other CDN-layer faults

- **Token authentication**: signed URLs with an expiry. Clock skew between the
  signer and the validator rejects valid requests; a misconfigured TTL rejects
  them after N minutes of playback.
- **Multi-CDN steering**: DNS or manifest-level switching between providers. A
  probe hitting one CDN says nothing about the other, which is why vantage
  labelling matters.
- **Geo and rate limiting**: a probe polling aggressively gets rate-limited, and
  the resulting 429 looks like an outage. (This happened during development here,
  which is why the demo polls politely.)
- **Range request support**: required for `SegmentBase` DASH and `EXT-X-MAP`
  byte ranges. A cache that mishandles ranges breaks those and nothing else.

### What goes wrong here

| Symptom | What is actually wrong |
|---|---|
| Stream advances in jumps | Manifest cached too long |
| Frozen for some viewers, fine for others | One edge stale; the others fine |
| Everything 403s after a while | Token expiry or clock skew |
| Fine from the office, broken for viewers | Different edge, different region, different CDN |
| High TTFB on first play, fine after | Cache miss reaching a JIT origin |

**StreamPulse:** cache attribution on every manifest finding, `manifest_fetch`,
`segment_availability`, `manifest_age_seconds` as a metric, and the
[vantage](../README.md#multi-vantage) label so the same channel probed from two
regions produces two independent answers rather than overwriting each other.

---

## 8. Ad insertion

Worth its own section because it is where most live-stream faults originate in
practice.

- **Client-side (CSAI)**: the player fetches ads from an ad server and switches
  to them. Easy to block, easy to get wrong on TVs.
- **Server-side (SSAI)**: a stitcher rewrites the manifest so ads and content are
  one continuous stream. Unblockable, and the source of a large fraction of
  production incidents.

Ad opportunities are signalled with **SCTE-35** markers, carried in-band in TS
or as `EXT-X-DATERANGE` / DASH events. A stitcher sees the marker, calls an ad
decision server, and splices the returned creatives in.

What it has to get right: in HLS, an `EXT-X-DISCONTINUITY` before and after the
break, correct `EXTINF` durations for the ad segments, and a media sequence that
keeps advancing. In DASH, a new `Period` with a correct `@start`, and a
`@duration` on the period it interrupted that lands exactly where the new one
begins.

When the arithmetic is off by a frame — or by the length of the entire break —
the presentation has a hole in it or two periods claiming the same instant.
Players stall at the boundary, which viewers experience as **the stream dying at
exactly the moment the ad starts**. It is also the most under-monitored failure
in streaming, because the content before and after the break is perfectly
healthy.

**StreamPulse:** `period_gap`, `period_overlap`, `discontinuity_present` (info —
splice points are worth knowing about even when correct), and the timeline
checks, which catch the same class of arithmetic error inside a period.

---

## 9. Where it breaks: a summary

| Layer | Fails as | Looks like | Attribution |
|---|---|---|---|
| Encoder | Falls behind real time | Live edge drifting further behind wall clock | `edge_stale` / `pdt_stale`, lag growing steadily |
| Encoder | Rung dies | Black or frozen picture on one rung only | `black_frames`, `frozen_video`, other rungs fine |
| Packager | Stops publishing | Manifest byte-identical, media sequence static | `playlist_stalled`, cache headers say the response is fresh |
| Packager | Bad splice | Stall at an ad boundary | `period_gap`, `timeline_gap` |
| Packager | DRM config lost | Clear segments on a protected stream | `unexpected_clear_segments` |
| Origin | Failover to a lagging instance | Timeline goes backwards | `playlist_rollback` |
| Origin | Storage purge | DVR shorter than promised | `window_below_declared`, `short_window` |
| CDN | Stale edge | Manifest frozen — **identical to a dead packager** | `Age` ≥ freeze duration; `X-Cache: STALE` |
| CDN | Manifest TTL too long | Stream advances in jumps | `manifest_age_seconds` climbing on a live stream |
| CDN | Whole-segment buffering | Latency far above target, nothing else wrong | `chunked_delivery_missing` |
| CDN | Missing object | One segment 404s, others fine | `segment_availability` |
| Manifest | Structurally wrong | Plays on most devices, fails on some | the structural checks in [§4](#4-hls) and [§5](#5-dash) |

The pattern worth internalising: **the faults that matter most are the ones
where every individual request succeeds.** A 404 is easy — any uptime checker
finds it. A stream that is perfectly available, perfectly fresh, fully
structurally valid, and fifteen seconds behind where it should be is the one
that survives a monitoring stack and reaches the viewer.

---

## Further reading

- [RFC 8216](https://datatracker.ietf.org/doc/html/rfc8216) — HLS, the original
  specification. The living version is Apple's `draft-pantos-hls-rfc8216bis`.
- [HLS Authoring Specification for Apple
  Devices](https://developer.apple.com/documentation/http-live-streaming/hls-authoring-specification-for-apple-devices)
  — ladder requirements, and what the App Store enforces.
- ISO/IEC 23009-1 — DASH. Paywalled; the
  [DASH-IF Interoperability Guidelines](https://dashif.org/guidelines/) are free
  and more useful in practice.
- [CTA-5005 / CMAF](https://www.cta.tech/) — the common container.
- [DASH-IF live simulator](https://livesim2.dashif.org/) — generates live DASH
  with configurable faults: multi-period, timeline addressing, segment loss. It
  is how several of the DASH checks here were developed. Poll it politely.
- [Unified Streaming demo streams](https://demo.unified-streaming.com/) — live
  HLS and DASH from a real packager, used throughout this project's testing.
