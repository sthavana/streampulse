package hls

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// Well-known DRM system IDs, as they appear in a PSSH box and (as a
// urn:uuid: KEYFORMAT) in an EXT-X-KEY tag.
const (
	SystemWidevine  = "edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"
	SystemPlayReady = "9a04f079-9840-4286-ab92-e65be0885f95"
	SystemFairPlay  = "94ce86fb-07ff-4f43-adb8-93d2fa968ca2"
	SystemCENC      = "1077efec-c0b2-4d02-ace3-3c1e52e2fb4b"
)

// KeyFormatFairPlay is the KEYFORMAT Apple uses for FairPlay Streaming.
const KeyFormatFairPlay = "com.apple.streamingkeydelivery"

var drmNames = map[string]string{
	SystemWidevine:    "Widevine",
	SystemPlayReady:   "PlayReady",
	SystemFairPlay:    "FairPlay",
	SystemCENC:        "Common Encryption",
	KeyFormatFairPlay: "FairPlay",
	"identity":        "AES-128 (identity)",
	"":                "AES-128 (identity)",
}

// Key is one EXT-X-KEY or EXT-X-SESSION-KEY tag (RFC 8216 4.3.2.4).
type Key struct {
	Method            string
	URI               string
	IV                string
	KeyFormat         string
	KeyFormatVersions string
}

// Encrypted reports whether this key actually protects anything. METHOD=NONE
// is a real tag meaning "segments from here on are in the clear".
func (k Key) Encrypted() bool {
	return k.Method != "" && !strings.EqualFold(k.Method, "NONE")
}

// Identity reports whether this is plain AES-128 with the key served directly
// at the URI, rather than a DRM system that brokers a licence. KEYFORMAT is
// defined to default to "identity" when absent.
func (k Key) Identity() bool {
	return k.KeyFormat == "" || strings.EqualFold(k.KeyFormat, "identity")
}

// System names the DRM system this key belongs to, for use in findings.
func (k Key) System() string {
	kf := strings.ToLower(strings.TrimPrefix(strings.ToLower(k.KeyFormat), "urn:uuid:"))
	if name, ok := drmNames[kf]; ok {
		return name
	}
	return "unknown (" + k.KeyFormat + ")"
}

// Fetchable reports whether the key URI can be retrieved over HTTP. FairPlay
// uses skd:// and Widevine/PlayReady commonly embed the PSSH in a data: URI;
// neither is an HTTP resource, and probing them as one would be wrong.
//
// A relative URI has no scheme yet -- EXT-X-KEY:URI="key.bin" is entirely
// normal -- and resolves against the playlist URL, so it is fetchable. Testing
// for an "http" prefix on the raw attribute would skip every relative key.
func (k Key) Fetchable() bool {
	if k.URI == "" {
		return false
	}
	u, err := url.Parse(k.URI)
	if err != nil {
		return false
	}
	if u.Scheme == "" {
		return true
	}
	return strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")
}

// DataURI decodes a base64 data: URI payload, which is how Widevine and
// PlayReady initialisation data is usually carried in an HLS playlist.
func (k Key) DataURI() ([]byte, bool) {
	const marker = "base64,"
	if !strings.HasPrefix(strings.ToLower(k.URI), "data:") {
		return nil, false
	}
	i := strings.Index(k.URI, marker)
	if i < 0 {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(k.URI[i+len(marker):]))
	if err != nil {
		return nil, false
	}
	return raw, true
}

// ValidIV reports whether the IV attribute is well formed: RFC 8216 requires a
// 128-bit value written as 0x followed by 32 hex digits. An absent IV is legal
// for AES-128 (the media sequence number is used instead), so "" is valid.
func ValidIV(iv string) bool {
	if iv == "" {
		return true
	}
	if len(iv) != 34 || !strings.EqualFold(iv[:2], "0x") {
		return false
	}
	_, err := hex.DecodeString(iv[2:])
	return err == nil
}

// PSSH is a parsed Protection System Specific Header box.
type PSSH struct {
	Version  byte
	SystemID string
	KIDs     []string
	DataSize int
}

// System names the DRM system, or reports the raw id when unrecognised.
func (p PSSH) System() string {
	if name, ok := drmNames[p.SystemID]; ok {
		return name
	}
	return "unknown (" + p.SystemID + ")"
}

// LooksLikePSSH reports whether b carries the 'pssh' box signature. Not every
// data: URI holds one -- PlayReady often carries a bare PlayReady Object -- so
// callers must check this before treating a parse failure as a defect.
func LooksLikePSSH(b []byte) bool {
	return len(b) >= 8 && string(b[4:8]) == "pssh"
}

// ParsePSSH reads a PSSH box (ISO/IEC 23001-7).
func ParsePSSH(b []byte) (PSSH, error) {
	var p PSSH
	if len(b) < 32 {
		return p, fmt.Errorf("too short for a PSSH box: %d bytes", len(b))
	}
	if string(b[4:8]) != "pssh" {
		return p, fmt.Errorf("missing 'pssh' box signature")
	}
	size := int(be32(b[0:4]))
	if size != len(b) {
		return p, fmt.Errorf("box size field is %d but the payload is %d bytes", size, len(b))
	}

	p.Version = b[8]
	p.SystemID = formatUUID(b[12:28])

	off := 28
	if p.Version > 0 {
		if len(b) < off+4 {
			return p, fmt.Errorf("truncated before the KID count")
		}
		n := int(be32(b[off : off+4]))
		off += 4
		if n < 0 || len(b) < off+16*n {
			return p, fmt.Errorf("declares %d KIDs but the box is too short", n)
		}
		for i := 0; i < n; i++ {
			p.KIDs = append(p.KIDs, formatUUID(b[off:off+16]))
			off += 16
		}
	}
	if len(b) < off+4 {
		return p, fmt.Errorf("truncated before the data size")
	}
	p.DataSize = int(be32(b[off : off+4]))
	off += 4
	if len(b) < off+p.DataSize {
		return p, fmt.Errorf("declares %d bytes of data but only %d remain", p.DataSize, len(b)-off)
	}
	return p, nil
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func formatUUID(b []byte) string {
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// parseKey builds a Key from an EXT-X-KEY / EXT-X-SESSION-KEY attribute list.
func parseKey(attrs map[string]string) Key {
	return Key{
		Method:            attrs["METHOD"],
		URI:               attrs["URI"],
		IV:                attrs["IV"],
		KeyFormat:         attrs["KEYFORMAT"],
		KeyFormatVersions: attrs["KEYFORMATVERSIONS"],
	}
}
