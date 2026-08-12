package mongodb

import (
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// rawToken builds a {"_data": <hex>} resume-token document.
func rawToken(t *testing.T, hexData string) bson.Raw {
	t.Helper()
	raw, err := bson.Marshal(bson.D{{Key: "_data", Value: hexData}})
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}
	return raw
}

// TestClusterTimeFromToken pins the Checkpoint anchor's clock domain: the anchor is
// decoded from the token's KeyString payload (0x82 marker + big-endian
// bson.Timestamp), i.e. SERVER time — never the client clock, whose skew
// would poison SaveToken's monotone guard.
func TestClusterTimeFromToken(t *testing.T) {
	const secs = uint32(1752444000) // 2025-07-13T22:00:00Z
	payload := make([]byte, 9)
	payload[0] = resumeTokenTimestampMarker
	binary.BigEndian.PutUint32(payload[1:5], secs)
	binary.BigEndian.PutUint32(payload[5:9], 7) // increment, ignored

	ct, ok := clusterTimeFromToken(rawToken(t, hex.EncodeToString(payload)))
	if !ok {
		t.Fatal("clusterTimeFromToken: ok = false for a well-formed token")
	}
	if want := time.Unix(int64(secs), 0).UTC(); !ct.Equal(want) {
		t.Fatalf("clusterTime = %v, want %v", ct, want)
	}
}

// TestClusterTimeFromTokenBestEffort proves decoding fails soft (ok=false, no
// panic) on every malformed shape, so Checkpoint falls back instead of erroring.
func TestClusterTimeFromTokenBestEffort(t *testing.T) {
	cases := map[string]bson.Raw{
		"missing _data":  mustRaw(t, bson.D{{Key: "other", Value: "x"}}),
		"non-string":     mustRaw(t, bson.D{{Key: "_data", Value: int32(1)}}),
		"non-hex":        rawToken(t, "zz"),
		"too short":      rawToken(t, "8201"),
		"wrong marker":   rawToken(t, "7f0000000100000001"),
		"empty payload":  rawToken(t, ""),
		"malformed bson": {0x01, 0x02},
	}
	for name, tok := range cases {
		if _, ok := clusterTimeFromToken(tok); ok {
			t.Errorf("%s: ok = true, want false", name)
		}
	}
}

func mustRaw(t *testing.T, d bson.D) bson.Raw {
	t.Helper()
	raw, err := bson.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// TestTokenKeyFromStringOrdersSameSecondTokens pins SaveToken's fine-grained
// ordering key: two tokens sharing the clusterTime SECOND but differing in the
// increment must produce keys whose lexicographic order matches the server's
// token order — the tie the coarse cluster_time anchor cannot see.
func TestTokenKeyFromStringOrdersSameSecondTokens(t *testing.T) {
	const secs = uint32(1752444000)
	mk := func(incr uint32) string {
		payload := make([]byte, 9)
		payload[0] = resumeTokenTimestampMarker
		binary.BigEndian.PutUint32(payload[1:5], secs)
		binary.BigEndian.PutUint32(payload[5:9], incr)
		return string(rawToken(t, hex.EncodeToString(payload)))
	}
	k1, ok1 := tokenKeyFromString(mk(1))
	k2, ok2 := tokenKeyFromString(mk(2))
	if !ok1 || !ok2 {
		t.Fatalf("ok = %v, %v for well-formed tokens, want true, true", ok1, ok2)
	}
	if k1 >= k2 {
		t.Fatalf("key order broken: key(incr=1) = %q not < key(incr=2) = %q", k1, k2)
	}
	// Case-insensitive: the same payload hex-encoded upper-case must yield the
	// same key (the store normalizes before comparing).
	upper, ok := tokenKeyFromString(string(rawToken(t, strings.ToUpper(hex.EncodeToString(func() []byte {
		p := make([]byte, 9)
		p[0] = resumeTokenTimestampMarker
		binary.BigEndian.PutUint32(p[1:5], secs)
		binary.BigEndian.PutUint32(p[5:9], 1)
		return p
	}())))))
	if !ok || upper != k1 {
		t.Fatalf("upper-case token key = %q ok=%v, want %q (case-normalized)", upper, ok, k1)
	}
}

// TestTokenKeyHexValidationMatchesStdlib pins the accept/reject set of the
// in-place hex check against hex.DecodeString, which it replaced: the decoded
// bytes were never used (only the hex string is stored and compared), so
// decoding allocated and discarded a buffer on every token save. Validating in
// place is only safe while it accepts EXACTLY what decoding accepted — a
// narrower check would silently drop real tokens to the coarse cluster_time
// guard, a wider one would let a non-hex payload through ToLower, which is
// order-preserving only over hex digits.
func TestTokenKeyHexValidationMatchesStdlib(t *testing.T) {
	for _, s := range []string{
		"00", "0123456789abcdef", "ABCDEF", "aBcDeF", "82ABCDEF01234567",
		"0", "000", "82ABC", // odd length
		"0g", "zz", "0x00", " 00", "00 ", "82-ab", "８２", // non-hex bytes
	} {
		_, decErr := hex.DecodeString(s)
		want := decErr == nil
		_, ok := tokenKeyFromString(string(rawToken(t, s)))
		if ok != want {
			t.Errorf("tokenKeyFromString(_data=%q) ok = %v, want %v (hex.DecodeString err = %v)", s, ok, want, decErr)
		}
	}
}

// TestTokenKeyFromStringBestEffort proves key extraction fails soft (ok=false)
// on every non-token shape, so SaveToken falls back to the coarse clusterTime
// guard instead of erroring.
func TestTokenKeyFromStringBestEffort(t *testing.T) {
	cases := map[string]string{
		"opaque string":  "tok1",
		"missing _data":  string(mustRaw(t, bson.D{{Key: "other", Value: "x"}})),
		"non-string":     string(mustRaw(t, bson.D{{Key: "_data", Value: int32(1)}})),
		"non-hex":        string(rawToken(t, "zz")),
		"empty payload":  string(rawToken(t, "")),
		"malformed bson": "\x01\x02",
		"empty":          "",
	}
	for name, tok := range cases {
		if _, ok := tokenKeyFromString(tok); ok {
			t.Errorf("%s: ok = true, want false", name)
		}
	}
}
