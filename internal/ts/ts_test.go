package ts

import "testing"

// pkt builds one 188-byte transport packet. Building them by hand is the only
// way to inject the faults this package exists to find: a real segment off a
// CDN is, encouragingly, never corrupt.
type pkt struct {
	pid           uint16
	cc            byte
	payloadStart  bool
	tei           bool
	noPayload     bool // adaptation field only
	discontinuity bool
	scrambled     bool
	pcr           bool
	body          []byte
}

func (p pkt) bytes() []byte {
	b := make([]byte, PacketSize)
	for i := range b {
		b[i] = 0xFF
	}
	b[0] = syncByte
	b[1] = byte(p.pid >> 8 & 0x1F)
	if p.payloadStart {
		b[1] |= 0x40
	}
	if p.tei {
		b[1] |= 0x80
	}
	b[2] = byte(p.pid & 0xFF)

	afc := byte(0x1) // payload only
	if p.noPayload {
		afc = 0x2
	} else if p.discontinuity || p.pcr {
		afc = 0x3 // both
	}
	b[3] = afc<<4 | (p.cc & 0x0F)
	if p.scrambled {
		b[3] |= 0x80
	}

	off := 4
	if afc&0x2 != 0 {
		b[off] = 1 // adaptation field length: just the flags byte
		var flags byte
		if p.discontinuity {
			flags |= 0x80
		}
		if p.pcr {
			flags |= 0x10
		}
		b[off+1] = flags
		off += 2
	}
	copy(b[off:], p.body)
	return b
}

func stream(ps ...pkt) []byte {
	var out []byte
	for _, p := range ps {
		out = append(out, p.bytes()...)
	}
	return out
}

// patPayload builds a program association table pointing at one PMT PID.
func patPayload(pmtPID uint16) []byte {
	body := []byte{
		0x00,                   // pointer field
		tableIDPAT, 0xB0, 0x0D, // table id, section syntax + length 13
		0x00, 0x01, // transport stream id
		0xC1, 0x00, 0x00, // version, section numbers
		0x00, 0x01, // program number 1
		byte(0xE0 | pmtPID>>8), byte(pmtPID & 0xFF),
		0x00, 0x00, 0x00, 0x00, // CRC
	}
	return body
}

// pmtPayload builds a program map table with one video and one audio stream.
func pmtPayload(pcrPID, videoPID, audioPID uint16) []byte {
	return []byte{
		0x00,
		tableIDPMT, 0xB0, 0x17, // length 23
		0x00, 0x01,
		0xC1, 0x00, 0x00,
		byte(0xE0 | pcrPID>>8), byte(pcrPID & 0xFF),
		0xF0, 0x00, // program info length 0
		0x1B, byte(0xE0 | videoPID>>8), byte(videoPID & 0xFF), 0xF0, 0x00, // H.264
		0x0F, byte(0xE0 | audioPID>>8), byte(audioPID & 0xFF), 0xF0, 0x00, // AAC
		0x00, 0x00, 0x00, 0x00,
	}
}

func healthy() []byte {
	return stream(
		pkt{pid: 0x0000, cc: 0, payloadStart: true, body: patPayload(0x20)},
		pkt{pid: 0x0020, cc: 0, payloadStart: true, body: pmtPayload(0x21, 0x21, 0x22)},
		pkt{pid: 0x0021, cc: 0, payloadStart: true, pcr: true},
		pkt{pid: 0x0021, cc: 1},
		pkt{pid: 0x0021, cc: 2},
		pkt{pid: 0x0022, cc: 0, payloadStart: true},
		pkt{pid: 0x0022, cc: 1},
	)
}

func TestHealthyStream(t *testing.T) {
	a := Analyse(healthy())
	if !a.Aligned || a.Truncated {
		t.Errorf("aligned=%v truncated=%v", a.Aligned, a.Truncated)
	}
	if a.Packets != 7 {
		t.Errorf("packets = %d, want 7", a.Packets)
	}
	for name, got := range map[string]int{
		"sync": a.SyncErrors, "transport": a.TransportErrors,
		"continuity": a.ContinuityErrors, "scrambled": a.Scrambled,
	} {
		if got != 0 {
			t.Errorf("%s errors = %d, want 0 on a healthy stream", name, got)
		}
	}
	if !a.HasPAT || !a.HasPMT {
		t.Errorf("PAT=%v PMT=%v, want both", a.HasPAT, a.HasPMT)
	}
	if len(a.Streams) != 2 || a.Streams[0].Kind() != "H.264" || a.Streams[1].Kind() != "AAC (ADTS)" {
		t.Errorf("streams = %+v", a.Streams)
	}
	if !a.HasPCR || a.PCRPID != 0x21 {
		t.Errorf("PCR = %v on 0x%04x, want true on 0x0021", a.HasPCR, a.PCRPID)
	}
}

// A gap in the continuity counter is a packet that went missing: TR 101 290
// priority 1, and a visible glitch for whoever is watching.
func TestContinuityGapIsAnError(t *testing.T) {
	b := stream(
		pkt{pid: 0x21, cc: 0, payloadStart: true},
		pkt{pid: 0x21, cc: 1},
		pkt{pid: 0x21, cc: 5}, // 2, 3 and 4 never arrived
		pkt{pid: 0x21, cc: 6},
	)
	a := Analyse(b)
	if a.ContinuityErrors != 1 {
		t.Errorf("continuity errors = %d, want 1", a.ContinuityErrors)
	}
	if st := a.PIDs[0x21]; st == nil || st.ContinuityErrors != 1 {
		t.Errorf("the error should be attributed to its PID, got %+v", st)
	}
}

func TestCounterWrapsWithoutError(t *testing.T) {
	b := stream(
		pkt{pid: 0x21, cc: 14, payloadStart: true},
		pkt{pid: 0x21, cc: 15},
		pkt{pid: 0x21, cc: 0}, // 15 -> 0 is the wrap, not a gap
		pkt{pid: 0x21, cc: 1},
	)
	if a := Analyse(b); a.ContinuityErrors != 0 {
		t.Errorf("a wrap was reported as %d errors", a.ContinuityErrors)
	}
}

// One duplicate packet is explicitly allowed by the spec; a second is not.
func TestDuplicatePacketsAllowedOnce(t *testing.T) {
	once := Analyse(stream(
		pkt{pid: 0x21, cc: 3, payloadStart: true},
		pkt{pid: 0x21, cc: 3},
		pkt{pid: 0x21, cc: 4},
	))
	if once.ContinuityErrors != 0 {
		t.Errorf("a single duplicate was reported as %d errors", once.ContinuityErrors)
	}
	twice := Analyse(stream(
		pkt{pid: 0x21, cc: 3, payloadStart: true},
		pkt{pid: 0x21, cc: 3},
		pkt{pid: 0x21, cc: 3},
	))
	if twice.ContinuityErrors != 1 {
		t.Errorf("a second duplicate = %d errors, want 1", twice.ContinuityErrors)
	}
}

// Adaptation-only packets do not advance the counter. Counting them would
// report a fault on every stream that pads, which is most of them.
func TestAdaptationOnlyPacketsDoNotAdvanceTheCounter(t *testing.T) {
	// Padding comes in bursts, and a packet with no payload repeats the last
	// counter rather than advancing it. Two in a row is what distinguishes
	// skipping them from letting the duplicate-packet rule absorb one.
	b := stream(
		pkt{pid: 0x21, cc: 7, payloadStart: true},
		pkt{pid: 0x21, cc: 7, noPayload: true},
		pkt{pid: 0x21, cc: 7, noPayload: true},
		pkt{pid: 0x21, cc: 8},
	)
	if a := Analyse(b); a.ContinuityErrors != 0 {
		t.Errorf("padding was reported as %d continuity errors", a.ContinuityErrors)
	}
}

// An announced discontinuity is a splice, not a fault. Reporting it would
// page someone at every ad break.
func TestAnnouncedDiscontinuityIsNotAnError(t *testing.T) {
	b := stream(
		pkt{pid: 0x21, cc: 5, payloadStart: true},
		pkt{pid: 0x21, cc: 12, discontinuity: true},
		pkt{pid: 0x21, cc: 13},
	)
	a := Analyse(b)
	if a.ContinuityErrors != 0 {
		t.Errorf("an announced discontinuity counted as %d errors", a.ContinuityErrors)
	}
	if a.Discontinuities != 1 {
		t.Errorf("discontinuities = %d, want 1", a.Discontinuities)
	}
}

func TestTransportErrorIndicator(t *testing.T) {
	b := stream(
		pkt{pid: 0x21, cc: 0, payloadStart: true},
		pkt{pid: 0x21, cc: 1, tei: true},
		pkt{pid: 0x21, cc: 2},
	)
	a := Analyse(b)
	if a.TransportErrors != 1 {
		t.Errorf("transport errors = %d, want 1", a.TransportErrors)
	}
	// A packet flagged as corrupt cannot have its counter trusted either, so
	// skipping it must not then produce a phantom continuity error.
	if a.ContinuityErrors != 0 {
		t.Errorf("a flagged packet produced %d continuity errors", a.ContinuityErrors)
	}
}

// A byte or two of leading rubbish is worth surviving rather than reporting
// as a stream of sync errors.
func TestLeadingGarbageIsResynced(t *testing.T) {
	b := append([]byte{0x00, 0x11, 0x22}, healthy()...)
	a := Analyse(b)
	if !a.Aligned {
		t.Fatal("failed to find the packet grid")
	}
	if a.SyncErrors != 0 {
		t.Errorf("sync errors = %d after resyncing", a.SyncErrors)
	}
	if a.Packets != 7 {
		t.Errorf("packets = %d, want 7", a.Packets)
	}
}

func TestNotATransportStream(t *testing.T) {
	a := Analyse([]byte("<html><body>404 Not Found</body></html>"))
	if a.Aligned {
		t.Error("an HTML error page should not align as a transport stream")
	}
	if a.Packets != 0 {
		t.Errorf("packets = %d, want 0", a.Packets)
	}
}

func TestTruncatedSegment(t *testing.T) {
	b := healthy()
	a := Analyse(b[:len(b)-40]) // cut mid-packet
	if !a.Truncated {
		t.Error("a segment cut mid-packet should report truncated")
	}
}

func TestNullPacketsAreIgnored(t *testing.T) {
	b := stream(
		pkt{pid: 0x21, cc: 0, payloadStart: true},
		pkt{pid: pidNull, cc: 0},
		pkt{pid: pidNull, cc: 9}, // null PID counters are meaningless
		pkt{pid: 0x21, cc: 1},
	)
	a := Analyse(b)
	if a.ContinuityErrors != 0 {
		t.Errorf("null packets produced %d continuity errors", a.ContinuityErrors)
	}
	if _, ok := a.PIDs[pidNull]; ok {
		t.Error("the null PID should not appear as a stream")
	}
}

func TestScramblingIsDetected(t *testing.T) {
	a := Analyse(stream(pkt{pid: 0x21, cc: 0, payloadStart: true, scrambled: true}))
	if a.Scrambled != 1 {
		t.Errorf("scrambled = %d, want 1", a.Scrambled)
	}
}

func TestMissingTables(t *testing.T) {
	a := Analyse(stream(pkt{pid: 0x21, cc: 0, payloadStart: true}))
	if a.HasPAT || a.HasPMT {
		t.Errorf("PAT=%v PMT=%v, want neither", a.HasPAT, a.HasPMT)
	}
}
