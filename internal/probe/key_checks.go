package probe

import (
	"context"
	"io"
	"net/http"
	"time"

	"streampulse/internal/alert"
	"streampulse/internal/config"
	"streampulse/internal/hls"
)

// aesKeyLen is the size of an AES-128 key served at an identity KEYFORMAT URI.
const aesKeyLen = 16

// keyChecks validates the EXT-X-KEY tags of one media playlist. It never puts
// key material into a finding: only the URI, the DRM system, and byte counts.
// The prober fetches real decryption keys, exactly as a player does, and those
// bytes must not reach a log line or a Slack channel.
func (p *Prober) keyChecks(ctx context.Context, t config.Target, plURL, variant string, pl *hls.MediaPlaylist) []alert.Finding {
	var out []alert.Finding
	now := p.now().UTC()
	labels := map[string]string{"target": t.Name, "variant": variant}

	distinct := pl.DistinctKeys()
	p.reg.SetGauge("streampulse_key_count", helpKeyCount,
		float64(len(distinct)), labels)

	// --- content expected to be encrypted but is not ---
	// Clear-lead leakage is the failure that actually matters commercially:
	// the stream plays, nothing 404s, and the content is unprotected.
	if t.ExpectEncrypted && len(pl.Segments) > 0 {
		if clear := pl.ClearSegments(); clear > 0 {
			out = append(out, finding(now, t, variant, alert.Critical, "unexpected_clear_segments",
				itoa(clear)+" of "+itoa(len(pl.Segments))+
					" segments carry no EXT-X-KEY on a stream declared encrypted"))
		}
	}

	// --- per-key structural checks ---
	for _, k := range pl.Keys {
		if !k.Encrypted() {
			continue
		}
		if k.URI == "" {
			out = append(out, finding(now, t, variant, alert.Critical, "key_missing_uri",
				"EXT-X-KEY METHOD="+k.Method+" has no URI, which the spec requires"))
			continue
		}
		if !hls.ValidIV(k.IV) {
			out = append(out, finding(now, t, variant, alert.Warning, "key_invalid_iv",
				"EXT-X-KEY IV "+quote(k.IV)+" is not 0x followed by 32 hex digits"))
		}
		if raw, ok := k.DataURI(); ok {
			out = append(out, psshChecks(now, t, variant, k, raw)...)
		}
	}

	// --- key availability ---
	if t.FetchKeys {
		out = append(out, p.fetchKeys(ctx, now, t, variant, plURL, distinct, labels)...)
	}

	// --- rotation ---
	out = append(out, p.rotationCheck(now, t, plURL, variant, distinct)...)

	return out
}

// psshChecks validates initialisation data carried in a data: URI. Only a
// payload that actually carries the 'pssh' signature is validated: PlayReady
// commonly ships a bare PlayReady Object instead, and treating that as a
// malformed box would be a false positive.
func psshChecks(now time.Time, t config.Target, variant string, k hls.Key, raw []byte) []alert.Finding {
	if !hls.LooksLikePSSH(raw) {
		return nil
	}
	box, err := hls.ParsePSSH(raw)
	if err != nil {
		return []alert.Finding{finding(now, t, variant, alert.Warning, "pssh_malformed",
			"PSSH box for "+k.System()+" failed to parse: "+err.Error())}
	}
	if box.DataSize == 0 && len(box.KIDs) == 0 {
		return []alert.Finding{finding(now, t, variant, alert.Warning, "pssh_empty",
			"PSSH box for "+box.System()+" carries no KIDs and no data")}
	}
	return nil
}

// fetchKeys retrieves each distinct key URI that is an HTTP resource. FairPlay
// (skd://) and embedded data: URIs are not fetchable and are skipped rather
// than reported as failures.
func (p *Prober) fetchKeys(ctx context.Context, now time.Time, t config.Target, variant, plURL string,
	keys []hls.Key, labels map[string]string) []alert.Finding {

	var out []alert.Finding
	for _, k := range keys {
		if !k.Fetchable() {
			continue
		}
		keyURL := resolveURL(plURL, k.URI)
		body, dur, status, err := p.fetchLimited(ctx, keyURL, 1024)
		p.reg.SetGauge("streampulse_key_fetch_seconds", "Time to fetch a key", dur.Seconds(), labels)

		if err != nil || status != http.StatusOK {
			p.reg.SetGauge("streampulse_key_available", "1 if the key URI is retrievable", 0, labels)
			detail := "HTTP " + itoa(status)
			if err != nil {
				detail = err.Error()
			}
			out = append(out, finding(now, t, variant, alert.Critical, "key_fetch",
				k.System()+" key not retrievable ("+detail+"): "+k.URI))
			continue
		}
		p.reg.SetGauge("streampulse_key_available", "1 if the key URI is retrievable", 1, labels)

		// An identity key is the raw AES-128 key: exactly 16 bytes. Anything
		// else means the URI is serving an error page or the wrong resource.
		// Only the length is reported; the bytes are never logged.
		if k.Identity() && len(body) != aesKeyLen {
			out = append(out, finding(now, t, variant, alert.Critical, "key_size_invalid",
				"identity key returned "+itoa(len(body))+" bytes, expected "+itoa(aesKeyLen)+": "+k.URI))
		}
	}
	return out
}

// rotationCheck flags a live stream whose keys have stopped rotating. Only
// meaningful when the operator states an expected interval: plenty of streams
// legitimately never rotate.
func (p *Prober) rotationCheck(now time.Time, t config.Target, plURL, variant string, keys []hls.Key) []alert.Finding {
	if t.KeyRotationMaxSec <= 0 || len(keys) == 0 {
		return nil
	}
	id := keys[len(keys)-1].Method + "|" + keys[len(keys)-1].URI + "|" + keys[len(keys)-1].KeyFormat

	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state[plURL]
	if st == nil {
		st = &plState{lastSeqChange: now}
		p.state[plURL] = st
	}
	if st.lastKeyID != id {
		st.lastKeyID = id
		st.lastKeyChange = now
		return nil
	}
	if st.lastKeyChange.IsZero() {
		st.lastKeyChange = now
		return nil
	}
	if stale := now.Sub(st.lastKeyChange); stale > time.Duration(t.KeyRotationMaxSec)*time.Second {
		return []alert.Finding{finding(now, t, variant, alert.Warning, "key_rotation_stalled",
			"encryption key unchanged for "+ftoa(stale.Seconds())+"s, expected rotation within "+
				itoa(t.KeyRotationMaxSec)+"s")}
	}
	return nil
}

// fetchLimited retrieves at most max bytes. Keys are tiny; this bounds what a
// misconfigured URI can pull into memory.
func (p *Prober) fetchLimited(ctx context.Context, u string, max int64) ([]byte, time.Duration, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, time.Since(start), 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, max))
	return b, time.Since(start), resp.StatusCode, err
}
