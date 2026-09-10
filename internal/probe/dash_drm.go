package probe

import (
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
	return out
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
