// Package ts reads MPEG transport stream packets well enough to answer the
// questions TR 101 290 Priority 1 asks of them.
//
// It is deliberately not a demuxer and not a TSDuck. What a prober needs from
// a downloaded segment is whether it is intact -- does it sync, did anything
// upstream flag corruption, did any packets go missing, are the tables a
// decoder needs present -- and all of that is in the four-byte packet header
// and two short sections. The measurements TSDuck exists for, PCR jitter and
// PTS repetition intervals, need a continuous stream and cannot be taken from
// an isolated segment by any tool.
package ts

const (
	// PacketSize is fixed at 188 bytes for broadcast and for HLS.
	PacketSize = 188
	syncByte   = 0x47

	pidPAT  = 0x0000
	pidNull = 0x1FFF

	tableIDPAT = 0x00
	tableIDPMT = 0x02
)

// Analysis is what one segment turned out to contain.
type Analysis struct {
	Packets int
	// SyncErrors counts packets whose sync byte is not 0x47 once the stream
	// has been aligned. A stream that never aligns reports Aligned false.
	SyncErrors int
	Aligned    bool
	// Truncated means the byte count is not a whole number of packets, which
	// is a segment that was cut off in transit or in production.
	Truncated bool
	// TransportErrors counts the transport_error_indicator, which is upstream
	// equipment saying "this packet arrived corrupt and I could not fix it".
	TransportErrors int
	// ContinuityErrors counts breaks in a PID's continuity counter that were
	// not announced by a discontinuity indicator: packets that went missing.
	ContinuityErrors int
	// Discontinuities counts announced ones, which are normal at a splice.
	Discontinuities int
	Scrambled       int

	HasPAT  bool
	HasPMT  bool
	PMTPIDs []uint16
	PCRPID  uint16
	HasPCR  bool
	// Streams are the elementary streams the PMT declares.
	Streams []ElementaryStream
	PIDs    map[uint16]*PIDStats
}

// ElementaryStream is one entry in the PMT.
type ElementaryStream struct {
	PID  uint16
	Type byte
}

// Kind names the stream type for a finding, for the types that appear in
// HLS. An unrecognised type is reported as its number rather than guessed at.
func (e ElementaryStream) Kind() string {
	switch e.Type {
	case 0x01, 0x02:
		return "MPEG-2 video"
	case 0x1B:
		return "H.264"
	case 0x24:
		return "HEVC"
	case 0x03, 0x04:
		return "MPEG audio"
	case 0x0F:
		return "AAC (ADTS)"
	case 0x11:
		return "AAC (LATM)"
	case 0x81:
		return "AC-3"
	case 0x87:
		return "E-AC-3"
	case 0x06:
		return "private data"
	case 0x15:
		return "metadata"
	default:
		return "type 0x" + hex2(e.Type)
	}
}

// PIDStats is the per-PID view, for naming which stream lost packets.
type PIDStats struct {
	PID              uint16
	Packets          int
	ContinuityErrors int
	Scrambled        bool
	HasPCR           bool

	lastCC   byte
	seenCC   bool
	lastDup  bool
	lastByte []byte
}

// Analyse reads a segment.
//
// It is tolerant by design: a segment that will not align at all is reported
// as such rather than producing thousands of spurious errors, and a section
// that cannot be parsed leaves its table simply not found.
func Analyse(b []byte) *Analysis {
	a := &Analysis{PIDs: map[uint16]*PIDStats{}}

	offset, ok := findSync(b)
	if !ok {
		return a
	}
	a.Aligned = true
	a.Truncated = (len(b)-offset)%PacketSize != 0

	pmtPIDs := map[uint16]bool{}

	for i := offset; i+PacketSize <= len(b); i += PacketSize {
		p := b[i : i+PacketSize]
		a.Packets++

		if p[0] != syncByte {
			a.SyncErrors++
			continue
		}
		if p[1]&0x80 != 0 {
			// transport_error_indicator: upstream equipment is saying this
			// packet arrived corrupt and it could not fix it.
			a.TransportErrors++
			// Its continuity counter cannot be trusted either, so the PID's
			// baseline is dropped rather than letting the gap it leaves
			// surface as a second, separate continuity error. TR 101 290
			// counts the two indicators separately; a monitoring tool
			// reporting one corrupt packet as two faults is just noise.
			if st := a.PIDs[uint16(p[1]&0x1F)<<8|uint16(p[2])]; st != nil {
				st.seenCC = false
			}
			continue
		}

		pid := uint16(p[1]&0x1F)<<8 | uint16(p[2])
		if pid == pidNull {
			continue
		}
		payloadStart := p[1]&0x40 != 0
		scrambling := p[3] >> 6
		afc := (p[3] >> 4) & 0x3
		cc := p[3] & 0x0F

		st := a.PIDs[pid]
		if st == nil {
			st = &PIDStats{PID: pid}
			a.PIDs[pid] = st
		}
		st.Packets++
		if scrambling != 0 {
			st.Scrambled = true
			a.Scrambled++
		}

		// The adaptation field carries the discontinuity indicator and the PCR.
		discontinuity := false
		payload := []byte(nil)
		off := 4
		if afc&0x2 != 0 {
			if off >= len(p) {
				continue
			}
			afLen := int(p[off])
			off++
			if afLen > 0 {
				if off+afLen > len(p) {
					continue
				}
				flags := p[off]
				discontinuity = flags&0x80 != 0
				if flags&0x10 != 0 { // PCR_flag
					st.HasPCR = true
					if !a.HasPCR {
						a.HasPCR, a.PCRPID = true, pid
					}
				}
			}
			off += afLen
		}
		if afc&0x1 != 0 && off < len(p) {
			payload = p[off:]
		}

		if discontinuity {
			a.Discontinuities++
			st.seenCC = false // the counter legitimately restarts here
		}

		// The continuity counter only advances on packets that carry payload.
		// Counting adaptation-only packets as gaps would report a fault on
		// every stream that pads.
		if afc&0x1 != 0 {
			if st.seenCC {
				switch {
				case cc == (st.lastCC+1)&0x0F:
					st.lastDup = false
				case cc == st.lastCC && !st.lastDup:
					// One duplicate packet is explicitly allowed; a second is
					// not (ISO/IEC 13818-1 2.4.3.3).
					st.lastDup = true
				default:
					st.ContinuityErrors++
					a.ContinuityErrors++
					st.lastDup = false
				}
			}
			st.lastCC, st.seenCC = cc, true
		}

		if len(payload) == 0 || !payloadStart {
			continue
		}
		switch {
		case pid == pidPAT:
			if pids, ok := parsePAT(payload); ok {
				a.HasPAT = true
				for _, pmt := range pids {
					if !pmtPIDs[pmt] {
						pmtPIDs[pmt] = true
						a.PMTPIDs = append(a.PMTPIDs, pmt)
					}
				}
			}
		case pmtPIDs[pid]:
			if streams, pcr, ok := parsePMT(payload); ok {
				a.HasPMT = true
				a.Streams = append(a.Streams, streams...)
				if pcr != 0 && pcr != pidNull {
					a.PCRPID = pcr
				}
			}
		}
	}
	return a
}

// findSync locates the packet grid.
//
// A segment normally starts on a packet boundary, but a byte or two of
// leading rubbish is exactly the sort of thing worth surviving rather than
// reporting as a stream of sync errors, so the first 188 offsets are tried
// and the one where the sync byte recurs on the grid wins.
func findSync(b []byte) (int, bool) {
	const confirm = 4
	for off := 0; off < PacketSize && off < len(b); off++ {
		n := 0
		for i := off; i+1 <= len(b) && n < confirm; i += PacketSize {
			if b[i] != syncByte {
				n = -1
				break
			}
			n++
		}
		if n > 0 {
			return off, true
		}
	}
	return 0, false
}

// section skips the pointer field and returns the table section.
func section(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	ptr := int(payload[0])
	if 1+ptr >= len(payload) {
		return nil
	}
	return payload[1+ptr:]
}

// parsePAT reads the program association table and returns the PMT PIDs.
//
// Only a section that fits in its first packet is read. A PAT that spans
// packets is legal and vanishingly rare in a segment this size, and half a
// table is worse than no table.
func parsePAT(payload []byte) ([]uint16, bool) {
	s := section(payload)
	if len(s) < 8 || s[0] != tableIDPAT {
		return nil, false
	}
	length := int(s[1]&0x0F)<<8 | int(s[2])
	end := 3 + length
	if end > len(s) || length < 9 {
		return nil, false
	}
	var out []uint16
	// 5 bytes of header after the length, 4 of CRC before the end.
	for i := 8; i+4 <= end-4; i += 4 {
		program := uint16(s[i])<<8 | uint16(s[i+1])
		pid := uint16(s[i+2]&0x1F)<<8 | uint16(s[i+3])
		if program != 0 { // program 0 is the network PID, not a PMT
			out = append(out, pid)
		}
	}
	return out, true
}

// parsePMT reads the program map table: the PCR PID and the elementary streams.
func parsePMT(payload []byte) ([]ElementaryStream, uint16, bool) {
	s := section(payload)
	if len(s) < 12 || s[0] != tableIDPMT {
		return nil, 0, false
	}
	length := int(s[1]&0x0F)<<8 | int(s[2])
	end := 3 + length
	if end > len(s) || length < 13 {
		return nil, 0, false
	}
	pcr := uint16(s[8]&0x1F)<<8 | uint16(s[9])
	infoLen := int(s[10]&0x0F)<<8 | int(s[11])
	i := 12 + infoLen
	var out []ElementaryStream
	for i+5 <= end-4 {
		typ := s[i]
		pid := uint16(s[i+1]&0x1F)<<8 | uint16(s[i+2])
		esLen := int(s[i+3]&0x0F)<<8 | int(s[i+4])
		out = append(out, ElementaryStream{PID: pid, Type: typ})
		i += 5 + esLen
	}
	return out, pcr, true
}

func hex2(b byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[b>>4], digits[b&0x0F]})
}
