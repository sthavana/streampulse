package probe

import (
	"context"
	"os"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/ts"
)

// analyseTransport reads a downloaded transport stream segment and reports the
// TR 101 290 Priority 1 faults that are answerable from one.
//
// This is the only deep check with no external dependency, so it runs in the
// 9MB image as well as the full one. It applies to transport stream segments
// only: every fMP4 stream -- all of DASH, and most modern HLS -- has no
// transport stream to analyse, and this abstains for them rather than
// reporting an absence as a fault.
func (p *Prober) analyseTransport(ctx context.Context, t config.Target, variant, segmentURL string) {
	if !t.TSAnalysis || segmentURL == "" {
		return
	}
	labels := map[string]string{"target": t.Name, "variant": variant}

	path, err := p.download(ctx, t, segmentURL, span{})
	if err != nil {
		// Reachability is the segment sample's business.
		return
	}
	defer os.Remove(path)

	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	a := ts.Analyse(b)

	if !a.Aligned {
		p.reg.SetGauge("streampulse_ts_aligned", helpTSAligned, 0, labels)
		p.emit(t.Name, variant, alert.Critical, "ts_sync_loss",
			"segment does not align as a transport stream: no 0x47 sync pattern found in "+
				itoa(len(b))+" bytes")
		return
	}
	p.reg.SetGauge("streampulse_ts_aligned", helpTSAligned, 1, labels)
	p.reg.SetGauge("streampulse_ts_packets", helpTSPackets, float64(a.Packets), labels)
	p.reg.SetGauge("streampulse_ts_continuity_errors", helpTSContinuity, float64(a.ContinuityErrors), labels)
	p.reg.SetGauge("streampulse_ts_transport_errors", helpTSTransport, float64(a.TransportErrors), labels)

	if a.Truncated {
		p.emit(t.Name, variant, alert.Critical, "ts_truncated",
			"segment ends mid-packet: "+itoa(len(b))+" bytes is not a whole number of 188-byte packets")
	}
	if a.SyncErrors > 0 {
		p.emit(t.Name, variant, alert.Critical, "ts_sync_loss",
			itoa(a.SyncErrors)+" of "+itoa(a.Packets)+" packets lost sync")
	}
	if a.TransportErrors > 0 {
		p.emit(t.Name, variant, alert.Critical, "ts_transport_errors",
			itoa(a.TransportErrors)+" of "+itoa(a.Packets)+
				" packets carry the transport error indicator: upstream equipment could not correct them")
	}
	if a.ContinuityErrors > 0 {
		p.emit(t.Name, variant, alert.Critical, "ts_continuity_errors",
			itoa(a.ContinuityErrors)+" continuity counter break(s) across "+itoa(len(a.PIDs))+
				" PID(s): packets went missing"+worstPID(a))
	}
	// A decoder cannot start without these, so their absence is as fatal as a
	// missing initialisation segment on the fMP4 side.
	if !a.HasPAT {
		p.emit(t.Name, variant, alert.Critical, "ts_pat_missing",
			"segment carries no program association table")
	}
	if a.HasPAT && !a.HasPMT {
		p.emit(t.Name, variant, alert.Critical, "ts_pmt_missing",
			"segment carries no program map table for the program its PAT declares")
	}
}

// worstPID names the PID that lost the most packets, because "something lost
// packets" sends an operator looking and "the video PID lost packets" tells
// them where.
func worstPID(a *ts.Analysis) string {
	var worst uint16
	most := 0
	for pid, st := range a.PIDs {
		if st.ContinuityErrors > most {
			worst, most = pid, st.ContinuityErrors
		}
	}
	if most == 0 {
		return ""
	}
	kind := ""
	for _, s := range a.Streams {
		if s.PID == worst {
			kind = " " + s.Kind()
			break
		}
	}
	return ", worst on PID 0x" + hex4(worst) + kind
}

func hex4(v uint16) string {
	const digits = "0123456789abcdef"
	return string([]byte{
		digits[v>>12&0xF], digits[v>>8&0xF], digits[v>>4&0xF], digits[v&0xF],
	})
}
