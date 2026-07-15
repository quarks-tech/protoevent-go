package mongodb

import (
	"encoding/binary"
	"encoding/hex"
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
