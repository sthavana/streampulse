package probe

import (
	"sort"
	"strings"
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/dash"
	"streampulse/internal/hls"
)

// dashDRMChecks validates the encryption a representation declares.
//
// DASH gives a prober less to work with than HLS does. An EXT-X-KEY carries a
// URI that a player fetches, so StreamPulse can prove the key is retrievable;
// an MPD carries no such thing. Licence acquisition happens over a DRM-system
// protocol with a signed challenge, which no synthetic prober can stand in
// for. So what is checkable here is what the manifest asserts: that the
// content is protected at all, and that the initialisation data it ships is
// well formed.
func (p *Prober) dashDRMChecks(now time.Time, t config.Target, r *dash.Representation, variant string) []alert.Finding {
	var out []alert.Finding
	labels := map[string]string{"target": t.Name, "variant": variant}

	prots := r.ContentProtections
	p.reg.SetGauge("streampulse_key_count", helpKeyCount, float64(distinctSystems(prots)), labels)

	// Clear-lead leakage: content going out unprotected on a stream that is
	// supposed to be encrypted. Nothing 404s and it plays perfectly, which is
	// exactly why it goes unnoticed.
	if t.ExpectEncrypted && len(prots) == 0 {
		out = append(out, finding(now, t, variant, alert.Critical, "unexpected_clear_segments",
			"expected an encrypted stream but the representation declares no ContentProtection"))
	}

	for _, c := range prots {
		out = append(out, protectionChecks(now, t, variant, c)...)
	}
	return append(out, p.dashRotationCheck(now, t, edgeKey(t, variant), variant, prots)...)
}

// dashRotationCheck flags a stream whose declared keys have stopped rotating.
//
// The HLS counterpart watches the EXT-X-KEY URI change; here the identity is
// what the manifest asserts about its own encryption -- the key ids and the
// initialisation data. A packager rotating keys republishes both, so a
// presentation whose ContentProtection is byte-identical poll after poll is
// one whose rotation has stopped. That matters because the point of rotation
// is to bound what a leaked key is worth, and it fails silently: the stream
// plays, every licence request succeeds, and the window a compromised key
// opens simply stops closing.
//
// Opt-in, through the same key_rotation_max_seconds as HLS, and for a sharper
// reason than "plenty of streams never rotate". DASH keys often rotate
// *in-band*, in the pssh of each segment's moof box, with the MPD never
// changing -- and a manifest probe cannot see that at all. Setting the knob is
// the operator saying their rotation is supposed to be visible in the
// manifest; without that assertion, silence here means nothing either way.
func (p *Prober) dashRotationCheck(now time.Time, t config.Target, key, variant string,
	prots []dash.ContentProtection) []alert.Finding {

	if t.KeyRotationMaxSec <= 0 || len(prots) == 0 {
		return nil
	}
	id := protectionIdentity(prots)

	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.edges[key]
	if st == nil {
		st = &edgeState{}
		p.edges[key] = st
	}
	if st.lastKeyID != id {
		st.lastKeyID, st.lastKeyChange = id, now
		return nil
	}
	if st.lastKeyChange.IsZero() {
		st.lastKeyChange = now
		return nil
	}
	stale := now.Sub(st.lastKeyChange)
	if stale <= time.Duration(t.KeyRotationMaxSec)*time.Second {
		return nil
	}
	return []alert.Finding{finding(now, t, variant, alert.Warning, "key_rotation_stalled",
		"declared encryption unchanged for "+ftoa(stale.Seconds())+"s, expected rotation within "+
			itoa(t.KeyRotationMaxSec)+"s")}
}

// protectionIdentity renders what a representation declares about its
// encryption as one comparable string.
//
// Sorted, because the order ContentProtection elements appear in is the
// packager's business and a reordering is not a rotation -- comparing them in
// document order would report one every time the packager reshuffled its
// output. Both the key id and the initialisation data are included: some
// packagers rotate the pssh while leaving default_KID alone, and either
// changing is evidence that rotation is alive.
func protectionIdentity(prots []dash.ContentProtection) string {
	parts := make([]string, 0, len(prots))
	for _, c := range prots {
		parts = append(parts, strings.ToLower(c.SystemID())+"|"+
			strings.ToLower(strings.TrimSpace(c.DefaultKID))+"|"+
			strings.Join(strings.Fields(c.PSSH), ""))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func protectionChecks(now time.Time, t config.Target, variant string, c dash.ContentProtection) []alert.Finding {
	var out []alert.Finding
	system := drmName(c)

	if kid := strings.TrimSpace(c.DefaultKID); kid != "" && !validKID(kid) {
		out = append(out, finding(now, t, variant, alert.Warning, "kid_invalid",
			"default_KID for "+system+" is not a UUID: "+kid))
	}

	if strings.TrimSpace(c.PSSH) == "" {
		return out
	}
	raw, ok := c.PSSHBytes()
	if !ok {
		return append(out, finding(now, t, variant, alert.Warning, "pssh_malformed",
			"cenc:pssh for "+system+" is not valid base64"))
	}
	// Not every payload is a PSSH box -- PlayReady commonly ships a bare
	// PlayReady Object -- and flagging those would be a false positive.
	if !hls.LooksLikePSSH(raw) {
		return out
	}
	box, err := hls.ParsePSSH(raw)
	if err != nil {
		return append(out, finding(now, t, variant, alert.Warning, "pssh_malformed",
			"PSSH box for "+system+" failed to parse: "+err.Error()))
	}
	if len(box.KIDs) == 0 && box.DataSize == 0 {
		out = append(out, finding(now, t, variant, alert.Warning, "pssh_empty",
			"PSSH box for "+box.System()+" carries no KIDs and no data"))
	}
	return out
}

// distinctSystems counts the DRM systems in force, ignoring the
// mp4protection descriptor, which announces the encryption scheme (cenc,
// cbcs) rather than a system and would otherwise inflate every count by one.
func distinctSystems(prots []dash.ContentProtection) int {
	seen := map[string]bool{}
	for _, c := range prots {
		if id := c.SystemID(); id != "" {
			seen[id] = true
		}
	}
	return len(seen)
}

// drmName labels a ContentProtection for a finding, reusing the HLS package's
// table of well-known system ids so the two formats name Widevine the same way.
func drmName(c dash.ContentProtection) string {
	id := c.SystemID()
	if id == "" {
		return c.SchemeIDURI
	}
	return hls.Key{KeyFormat: "urn:uuid:" + id}.System()
}

// validKID accepts a KID in either the dashed UUID form the schema calls for
// or the bare 32 hex digits some packagers emit. The check is aimed at a
// default_KID that is not a key id at all -- empty-ish, truncated, or a stray
// template variable -- rather than at punctuation.
func validKID(kid string) bool {
	hexOnly := strings.ReplaceAll(kid, "-", "")
	if len(hexOnly) != 32 {
		return false
	}
	for i := 0; i < len(hexOnly); i++ {
		c := hexOnly[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
