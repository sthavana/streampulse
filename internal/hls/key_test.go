package hls

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func TestParseMediaKeys(t *testing.T) {
	raw := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-KEY:METHOD=AES-128,URI="https://keys.example/k1",IV=0x9c7db8778570d05c3c6a0e0dc4b0c1f0
#EXTINF:4.000,
seg0.ts
#EXTINF:4.000,
seg1.ts
#EXT-X-KEY:METHOD=AES-128,URI="https://keys.example/k2"
#EXTINF:4.000,
seg2.ts
#EXT-X-ENDLIST
`
	pl := ParseMedia(raw)
	if len(pl.Keys) != 2 {
		t.Fatalf("got %d key tags, want 2", len(pl.Keys))
	}
	if pl.Keys[0].Method != "AES-128" || pl.Keys[0].URI != "https://keys.example/k1" {
		t.Errorf("first key parsed wrong: %+v", pl.Keys[0])
	}
	if pl.Keys[0].IV != "0x9c7db8778570d05c3c6a0e0dc4b0c1f0" {
		t.Errorf("IV parsed wrong: %q", pl.Keys[0].IV)
	}

	// A key applies to every segment that follows it, until the next key.
	if len(pl.Segments) != 3 {
		t.Fatalf("got %d segments, want 3", len(pl.Segments))
	}
	for i, want := range []string{"https://keys.example/k1", "https://keys.example/k1", "https://keys.example/k2"} {
		if pl.Segments[i].Key == nil {
			t.Fatalf("segment %d has no key", i)
		}
		if pl.Segments[i].Key.URI != want {
			t.Errorf("segment %d key = %q, want %q", i, pl.Segments[i].Key.URI, want)
		}
	}
	if !pl.Encrypted() {
		t.Error("Encrypted() = false on an encrypted playlist")
	}
	if got := pl.ClearSegments(); got != 0 {
		t.Errorf("ClearSegments() = %d, want 0", got)
	}
}

// METHOD=NONE is not an absence of information: it marks a deliberate return
// to clear, and the segments after it must come back unencrypted.
func TestMethodNoneReturnsSegmentsToClear(t *testing.T) {
	raw := `#EXTM3U
#EXT-X-TARGETDURATION:4
#EXT-X-KEY:METHOD=AES-128,URI="https://keys.example/k1"
#EXTINF:4.000,
enc.ts
#EXT-X-KEY:METHOD=NONE
#EXTINF:4.000,
clear.ts
#EXT-X-ENDLIST
`
	pl := ParseMedia(raw)
	if pl.Segments[0].Key == nil {
		t.Error("first segment should be encrypted")
	}
	if pl.Segments[1].Key != nil {
		t.Error("segment after METHOD=NONE should be clear")
	}
	if got := pl.ClearSegments(); got != 1 {
		t.Errorf("ClearSegments() = %d, want 1", got)
	}
	// The NONE tag is still recorded, but is not an encrypting key.
	if len(pl.Keys) != 2 {
		t.Errorf("got %d key tags, want 2 including the NONE", len(pl.Keys))
	}
	if len(pl.DistinctKeys()) != 1 {
		t.Errorf("DistinctKeys() = %d, want 1", len(pl.DistinctKeys()))
	}
}

func TestUnencryptedPlaylistHasNoKeys(t *testing.T) {
	pl := ParseMedia("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\na.ts\n#EXT-X-ENDLIST\n")
	if pl.Encrypted() {
		t.Error("Encrypted() = true with no EXT-X-KEY")
	}
	if got := pl.ClearSegments(); got != 1 {
		t.Errorf("ClearSegments() = %d, want 1", got)
	}
}

func TestDistinctKeysDeduplicates(t *testing.T) {
	raw := `#EXTM3U
#EXT-X-TARGETDURATION:4
#EXT-X-KEY:METHOD=AES-128,URI="https://k/1"
#EXTINF:4.0,
a.ts
#EXT-X-KEY:METHOD=AES-128,URI="https://k/1"
#EXTINF:4.0,
b.ts
#EXT-X-KEY:METHOD=AES-128,URI="https://k/2"
#EXTINF:4.0,
c.ts
#EXT-X-ENDLIST
`
	if got := len(ParseMedia(raw).DistinctKeys()); got != 2 {
		t.Errorf("DistinctKeys() = %d, want 2", got)
	}
}

func TestParseSessionKey(t *testing.T) {
	raw := `#EXTM3U
#EXT-X-SESSION-KEY:METHOD=SAMPLE-AES,URI="skd://fps.example/abc",KEYFORMAT="com.apple.streamingkeydelivery",KEYFORMATVERSIONS="1"
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360
360p.m3u8
`
	m := ParseMaster(raw)
	if len(m.SessionKeys) != 1 {
		t.Fatalf("got %d session keys, want 1", len(m.SessionKeys))
	}
	k := m.SessionKeys[0]
	if k.Method != "SAMPLE-AES" || k.KeyFormat != KeyFormatFairPlay {
		t.Errorf("session key parsed wrong: %+v", k)
	}
	if k.System() != "FairPlay" {
		t.Errorf("System() = %q, want FairPlay", k.System())
	}
	if k.Fetchable() {
		t.Error("skd:// must not be treated as an HTTP resource")
	}
	if len(m.Variants) != 1 || m.Variants[0].URI != "360p.m3u8" {
		t.Errorf("session key disturbed variant parsing: %+v", m.Variants)
	}
}

// --- Key helpers ---

func TestKeyEncryptedAndIdentity(t *testing.T) {
	cases := []struct {
		k             Key
		enc, identity bool
	}{
		{Key{Method: "AES-128"}, true, true},
		{Key{Method: "NONE"}, false, true},
		{Key{Method: "none"}, false, true},
		{Key{}, false, true},
		{Key{Method: "SAMPLE-AES", KeyFormat: KeyFormatFairPlay}, true, false},
		{Key{Method: "AES-128", KeyFormat: "identity"}, true, true},
	}
	for _, c := range cases {
		if got := c.k.Encrypted(); got != c.enc {
			t.Errorf("%+v Encrypted() = %v, want %v", c.k, got, c.enc)
		}
		if got := c.k.Identity(); got != c.identity {
			t.Errorf("%+v Identity() = %v, want %v", c.k, got, c.identity)
		}
	}
}

func TestKeyFetchable(t *testing.T) {
	cases := map[string]bool{
		"https://keys.example/k1":     true,
		"http://keys.example/k1":      true,
		"HTTPS://keys.example/k1":     true,
		"skd://fps.example/abc":       false, // FairPlay
		"data:text/plain;base64,AAAA": false, // embedded init data
		"":                            false,

		// Relative key URIs are entirely normal and resolve against the
		// playlist URL. Requiring an http prefix on the raw attribute would
		// silently skip every one of them.
		"key.bin":          true,
		"../keys/k1":       true,
		"/drm/key?kid=abc": true,
	}
	for uri, want := range cases {
		if got := (Key{URI: uri}).Fetchable(); got != want {
			t.Errorf("Fetchable(%q) = %v, want %v", uri, got, want)
		}
	}
}

func TestKeySystemNaming(t *testing.T) {
	cases := map[string]string{
		"":                            "AES-128 (identity)",
		"identity":                    "AES-128 (identity)",
		KeyFormatFairPlay:             "FairPlay",
		"urn:uuid:" + SystemWidevine:  "Widevine",
		"urn:uuid:" + SystemPlayReady: "PlayReady",
		"URN:UUID:" + SystemWidevine:  "Widevine",
	}
	for kf, want := range cases {
		if got := (Key{KeyFormat: kf}).System(); got != want {
			t.Errorf("System(%q) = %q, want %q", kf, got, want)
		}
	}
}

func TestValidIV(t *testing.T) {
	valid := []string{"", "0x9c7db8778570d05c3c6a0e0dc4b0c1f0", "0X9C7DB8778570D05C3C6A0E0DC4B0C1F0"}
	for _, iv := range valid {
		if !ValidIV(iv) {
			t.Errorf("ValidIV(%q) = false, want true", iv)
		}
	}
	invalid := []string{
		"0x9c7db8",                            // too short
		"9c7db8778570d05c3c6a0e0dc4b0c1f0",    // missing 0x
		"0xZZ7db8778570d05c3c6a0e0dc4b0c1f0",  // not hex
		"0x9c7db8778570d05c3c6a0e0dc4b0c1f00", // too long
	}
	for _, iv := range invalid {
		if ValidIV(iv) {
			t.Errorf("ValidIV(%q) = true, want false", iv)
		}
	}
}

// --- PSSH ---

// psshBox builds a v0 PSSH box for the given system, with n bytes of data.
func psshBox(systemID string, data []byte) []byte {
	sid, _ := hex.DecodeString(strings.ReplaceAll(systemID, "-", ""))
	size := 32 + len(data)
	b := make([]byte, 0, size)
	b = append(b, byte(size>>24), byte(size>>16), byte(size>>8), byte(size))
	b = append(b, 'p', 's', 's', 'h')
	b = append(b, 0, 0, 0, 0) // version 0, no flags
	b = append(b, sid...)
	b = append(b, byte(len(data)>>24), byte(len(data)>>16), byte(len(data)>>8), byte(len(data)))
	b = append(b, data...)
	return b
}

func TestParsePSSHWidevine(t *testing.T) {
	raw := psshBox(SystemWidevine, []byte("some-init-data"))
	box, err := ParsePSSH(raw)
	if err != nil {
		t.Fatalf("ParsePSSH errored: %v", err)
	}
	if box.SystemID != SystemWidevine {
		t.Errorf("SystemID = %q, want %q", box.SystemID, SystemWidevine)
	}
	if box.System() != "Widevine" {
		t.Errorf("System() = %q, want Widevine", box.System())
	}
	if box.DataSize != len("some-init-data") {
		t.Errorf("DataSize = %d, want %d", box.DataSize, len("some-init-data"))
	}
}

func TestParsePSSHRejectsMalformed(t *testing.T) {
	good := psshBox(SystemWidevine, []byte("data"))

	cases := map[string][]byte{
		"too short":     good[:20],
		"bad signature": append([]byte{0, 0, 0, 32, 'x', 'y', 'z', 'w'}, good[8:]...),
		"size mismatch": append([]byte{0, 0, 1, 0}, good[4:]...),
	}
	for name, b := range cases {
		if _, err := ParsePSSH(b); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// A payload that is not a PSSH box at all must be recognised as such, so the
// caller can skip it rather than report a malformed box. PlayReady commonly
// ships a bare PlayReady Object in the data: URI.
func TestLooksLikePSSH(t *testing.T) {
	if !LooksLikePSSH(psshBox(SystemWidevine, []byte("d"))) {
		t.Error("a real PSSH box should be recognised")
	}
	if LooksLikePSSH([]byte("<WRMHEADER><DATA/></WRMHEADER>")) {
		t.Error("a PlayReady Object must not be mistaken for a PSSH box")
	}
	if LooksLikePSSH([]byte{1, 2}) {
		t.Error("a short payload is not a PSSH box")
	}
}

func TestDataURIDecoding(t *testing.T) {
	raw := psshBox(SystemWidevine, []byte("init"))
	k := Key{URI: "data:text/plain;base64," + base64.StdEncoding.EncodeToString(raw)}

	got, ok := k.DataURI()
	if !ok {
		t.Fatal("DataURI() returned false for a valid data: URI")
	}
	if string(got) != string(raw) {
		t.Error("decoded payload does not round-trip")
	}
	if _, ok := (Key{URI: "https://keys.example/k"}).DataURI(); ok {
		t.Error("an http URI is not a data: URI")
	}
	if _, ok := (Key{URI: "data:text/plain;base64,!!!not-base64!!!"}).DataURI(); ok {
		t.Error("undecodable base64 should report false")
	}
}
