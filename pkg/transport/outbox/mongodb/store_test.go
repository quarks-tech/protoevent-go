package mongodb_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	mongodbstore "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb/mongodbtest"
)

func TestNewStoreConstructs(t *testing.T) {
	if mongodbstore.NewStore(nil) == nil {
		t.Fatal("NewStore returned nil")
	}
}

func TestNewStoreWithRetention(t *testing.T) {
	if mongodbstore.NewStore(nil, mongodbstore.WithRetention(48*time.Hour)) == nil {
		t.Fatal("NewStore returned nil")
	}
}

// TestCreateOutboxMessageRejectsNilMetadata proves the nil-Metadata guard
// fires before any driver call: a nil *mongo.Database (via NewStore(nil))
// would panic on the first Collection(...) call if the guard didn't run
// first.
func TestCreateOutboxMessageRejectsNilMetadata(t *testing.T) {
	st := mongodbstore.NewStore(nil)
	err := st.CreateOutboxMessage(context.Background(), &outbox.Message{ID: "id"})
	if err == nil {
		t.Fatal("expected error for nil Metadata, got nil")
	}
}

// TestCreateOutboxMessageRejectsEmptyID proves the empty-ID guard fires before
// any driver call (same nil-*mongo.Database trick as the nil-Metadata test).
// Without it, an empty _id inserts fine ONCE and every later publish fails
// with a far-from-cause duplicate-key error.
func TestCreateOutboxMessageRejectsEmptyID(t *testing.T) {
	st := mongodbstore.NewStore(nil)
	err := st.CreateOutboxMessage(context.Background(), &outbox.Message{Metadata: event.NewMetadata("t")})
	if err == nil {
		t.Fatal("expected error for empty ID, got nil")
	}
}

// TestCreateOutboxMessageRejectsZeroCreateTime proves the TTL-anchor guard:
// create_time drives the TTL index, and a zero value (0001-01-01) is already
// past every retention window — the TTL monitor would silently reap the row
// before the relay drains it. Fires before any driver call (nil db).
func TestCreateOutboxMessageRejectsZeroCreateTime(t *testing.T) {
	st := mongodbstore.NewStore(nil)
	md := event.NewMetadata("books.created")
	md.ID = "id-1"
	err := st.CreateOutboxMessage(context.Background(), &outbox.Message{ID: md.ID, Metadata: md})
	if err == nil || !strings.Contains(err.Error(), "create time is zero") {
		t.Fatalf("err = %v, want descriptive zero-create-time error", err)
	}
}

// TestCreateOutboxMessageRejectsTransactionlessCtx proves the tx guard: a
// valid message published on a ctx with no running transaction is rejected
// (an outbox row committing independently of the business write would be a
// phantom event), before any driver call — same nil-*mongo.Database trick.
func TestCreateOutboxMessageRejectsTransactionlessCtx(t *testing.T) {
	st := mongodbstore.NewStore(nil)
	md := event.NewMetadata("t")
	md.Time = time.Now()
	err := st.CreateOutboxMessage(context.Background(), &outbox.Message{ID: "id", Metadata: md, CreateTime: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "transaction") {
		t.Fatalf("expected transactionless-ctx rejection, got %v", err)
	}
}

// TestCreateOutboxMessageRejectsZeroMetadataTime proves the write side of the
// zero-Time invariant the read side relies on: decodeMessage classifies
// zero-Time metadata as poison (the "{}" marker), so a publish carrying it
// must be rejected up front instead of planting a row the relay would park.
// Fires before any driver call (nil db).
func TestCreateOutboxMessageRejectsZeroMetadataTime(t *testing.T) {
	st := mongodbstore.NewStore(nil)
	err := st.CreateOutboxMessage(context.Background(), &outbox.Message{ID: "id", Metadata: event.NewMetadata("t"), CreateTime: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "metadata time is zero") {
		t.Fatalf("err = %v, want descriptive zero-metadata-time error", err)
	}
}

// TestEnsureIndexesRejectsSubSecondRetention proves the TTL range guard:
// expireAfterSeconds is whole int32 seconds, and a sub-second retention would
// truncate to 0 — expiring every row immediately. Fails before any driver
// call (nil *mongo.Database).
func TestEnsureIndexesRejectsSubSecondRetention(t *testing.T) {
	st := mongodbstore.NewStore(nil, mongodbstore.WithRetention(500*time.Millisecond))
	err := st.EnsureIndexes(context.Background())
	if err == nil || !strings.Contains(err.Error(), "retention") {
		t.Fatalf("expected retention range rejection, got %v", err)
	}
}

// TestEnsureIndexesRejectsNonPositiveRetention pins the WithRetention
// store-as-given decision: a zero (e.g. an unset config field) is a caller
// bug, not a request for the default, and must fail loudly at EnsureIndexes
// instead of being silently swallowed by the option.
func TestEnsureIndexesRejectsNonPositiveRetention(t *testing.T) {
	st := mongodbstore.NewStore(nil, mongodbstore.WithRetention(0))
	err := st.EnsureIndexes(context.Background())
	if err == nil || !strings.Contains(err.Error(), "retention") {
		t.Fatalf("expected retention rejection for 0, got %v", err)
	}
}

// TestEnsureIndexesRejectsFractionalRetention proves a retention that is not
// a whole number of seconds is rejected rather than silently truncated:
// 1500ms would otherwise become a 1s TTL, expiring rows earlier than the
// configured window.
func TestEnsureIndexesRejectsFractionalRetention(t *testing.T) {
	st := mongodbstore.NewStore(nil, mongodbstore.WithRetention(1500*time.Millisecond))
	err := st.EnsureIndexes(context.Background())
	if err == nil || !strings.Contains(err.Error(), "whole number of seconds") {
		t.Fatalf("expected fractional-retention rejection, got %v", err)
	}
}

// TestEnsureIndexesRejectsRetentionAboveTTLRange is the regression test for
// the upper bound: expireAfterSeconds is int32, and 2^31 seconds would
// overflow the cast.
func TestEnsureIndexesRejectsRetentionAboveTTLRange(t *testing.T) {
	st := mongodbstore.NewStore(nil, mongodbstore.WithRetention((1<<31)*time.Second))
	err := st.EnsureIndexes(context.Background())
	if err == nil || !strings.Contains(err.Error(), "retention") {
		t.Fatalf("expected retention range rejection, got %v", err)
	}
}

var testDB *mongo.Database

func TestMain(m *testing.M) {
	inst, cleanup, err := mongodbtest.Start(context.Background())
	if err != nil {
		if errors.Is(err, mongodbtest.ErrDockerUnavailable) {
			if os.Getenv("CI") != "" {
				// In CI a missing Docker must fail loudly: os.Exit(0) would
				// silently "pass" the whole module with zero tests run.
				fmt.Fprintf(os.Stderr, "CI is set but Docker is unavailable; refusing to skip mongo integration tests: %v\n", err)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "skipping mongo integration tests: %v\n", err)
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "mongo integration setup: %v\n", err)
		os.Exit(1) // real harness bug, not a missing Docker
	}
	testDB = inst.DB
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// reset and publish/publishMetadata below take testing.TB (rather than
// *testing.T) so both tests and benchmarks in this package can share them.
func reset(tb testing.TB) {
	tb.Helper()
	for _, c := range []string{"outbox_messages", "outbox_offsets", "relay_locks"} {
		if err := testDB.Collection(c).Drop(context.Background()); err != nil {
			tb.Fatalf("drop %s: %v", c, err)
		}
	}
}

func publish(tb testing.TB, subject string) string {
	tb.Helper()
	md := event.NewMetadata("books.created")
	md.ID = uuid.NewString()
	md.Source = "books-service"
	md.Subject = subject
	md.DataContentType = "application/proto"
	md.Time = time.Now().UTC()
	return publishMetadata(tb, md, []byte("x"))
}

// publishMetadata publishes a caller-prepared *event.Metadata through the
// production Sender (mirrors production: same session-txn helper as publish).
// Returns the outbox row/event ID.
func publishMetadata(tb testing.TB, md *event.Metadata, data []byte) string {
	tb.Helper()
	st := mongodbstore.NewStore(testDB)
	// Publish in a transaction (mirrors production; requires the replica set).
	sess, err := testDB.Client().StartSession()
	if err != nil {
		tb.Fatal(err)
	}
	defer sess.EndSession(context.Background())
	_, err = sess.WithTransaction(context.Background(), func(sc context.Context) (any, error) {
		// Go through the production Sender so the default ReuseMetadataID
		// generator (no explicit Message.ID) derives the row id from md.ID.
		return nil, outbox.NewSender(st).Send(sc, md, data)
	})
	if err != nil {
		tb.Fatalf("publish: %v", err)
	}
	return md.ID
}

func TestPublishInsertsEnvelope(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	id := publish(t, "s1")
	n, err := testDB.Collection("outbox_messages").CountDocuments(context.Background(), bson.M{"_id": id})
	if err != nil || n != 1 {
		t.Fatalf("count = %d err = %v, want 1", n, err)
	}
}

func TestEnsureIndexesCreatesTTL(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	if err := mongodbstore.NewStore(testDB).EnsureIndexes(context.Background()); err != nil {
		t.Fatalf("ensure indexes: %v", err)
	}
	cur, err := testDB.Collection("outbox_messages").Indexes().List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var idx []bson.M
	if err := cur.All(context.Background(), &idx); err != nil {
		t.Fatal(err)
	}
	// Same meaning as the unexported mongodb.defaultRetention (7 days, a
	// time.Duration); this is an external test package so it can't reference
	// the constant directly.
	const wantTTLSeconds = int32(7 * 24 * 60 * 60)
	found := false
	for _, ix := range idx {
		// Nested documents decode to bson.D (not bson.M) when the outer
		// document is unmarshaled into a bson.M.
		key, ok := ix["key"].(bson.D)
		if !ok {
			continue
		}
		hasCreateTime := false
		for _, e := range key {
			if e.Key == "create_time" {
				hasCreateTime = true
				break
			}
		}
		if !hasCreateTime {
			continue
		}
		ttl, ok := ix["expireAfterSeconds"]
		if !ok {
			continue
		}
		found = true
		var ttlSeconds int32
		switch v := ttl.(type) {
		case int32:
			ttlSeconds = v
		case int64:
			if v >= math.MinInt32 && v <= math.MaxInt32 {
				ttlSeconds = int32(v)
			} else {
				t.Fatalf("expireAfterSeconds %d overflows int32", v)
			}
		case float64:
			if v > math.MaxInt32 || v < math.MinInt32 {
				t.Fatalf("expireAfterSeconds %v overflows int32", v)
			}
			ttlSeconds = int32(v)
		default:
			t.Fatalf("expireAfterSeconds has unexpected type %T: %v", ttl, ttl)
		}
		if ttlSeconds != wantTTLSeconds {
			t.Fatalf("create_time TTL index expireAfterSeconds = %d, want %d", ttlSeconds, wantTTLSeconds)
		}
	}
	if !found {
		t.Fatal("no TTL index on outbox.create_time")
	}
}

// TestEnsureIndexesRetentionConflictHinted proves the WithRetention knob
// reaches the TTL index, and that changing retention on an EXISTING collection
// surfaces MongoDB's IndexOptionsConflict with a collMod hint instead of
// silently keeping the old TTL.
func TestEnsureIndexesRetentionConflictHinted(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	ctx := context.Background()

	if err := mongodbstore.NewStore(testDB, mongodbstore.WithRetention(48*time.Hour)).EnsureIndexes(ctx); err != nil {
		t.Fatalf("ensure indexes (48h): %v", err)
	}
	// Re-ensuring with the SAME retention stays idempotent.
	if err := mongodbstore.NewStore(testDB, mongodbstore.WithRetention(48*time.Hour)).EnsureIndexes(ctx); err != nil {
		t.Fatalf("re-ensure with unchanged retention: %v", err)
	}
	// A DIFFERENT retention on the existing index must fail loudly, with the
	// operational hint (collMod) in the message.
	err := mongodbstore.NewStore(testDB, mongodbstore.WithRetention(24*time.Hour)).EnsureIndexes(ctx)
	if err == nil {
		t.Fatal("re-ensure with a different retention succeeded; want IndexOptionsConflict surfaced")
	}
	if !strings.Contains(err.Error(), "collMod") {
		t.Fatalf("conflict error carries no collMod hint: %v", err)
	}
}

func TestOffsetRoundTrip(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx := context.Background()
	ct := time.Now().UTC().Truncate(time.Millisecond)
	if persisted, err := st.SaveToken(ctx, "c", "\x01\x02", ct); err != nil || !persisted {
		t.Fatalf("SaveToken: persisted=%v err=%v, want true, nil", persisted, err)
	}
	tok, gotCT, err := st.LoadToken(ctx, "c") // tok is string
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 2 || tok[0] != 0x01 || tok[1] != 0x02 {
		t.Fatalf("token = %q, want \\x01\\x02", tok)
	}
	if !gotCT.Equal(ct) {
		t.Fatalf("clusterTime = %v, want %v", gotCT, ct)
	}
}

// TestSaveTokenMonotoneClusterTime proves SaveToken honors the stream.Store
// contract's monotonicity clause: a save carrying an OLDER clusterTime than
// the stored row (a stale leader finishing a slow window) is a silent no-op,
// while a newer one still advances the position.
func TestSaveTokenMonotoneClusterTime(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx := context.Background()

	t1 := time.Now().UTC().Truncate(time.Millisecond)
	t0 := t1.Add(-time.Minute)
	t2 := t1.Add(time.Minute)

	if persisted, err := st.SaveToken(ctx, "c", "tok1", t1); err != nil || !persisted {
		t.Fatalf("save tok1: persisted=%v err=%v, want true, nil", persisted, err)
	}
	// Stale save: older clusterTime must not rewind the stored row — no error
	// (the skip is deliberate, not a failure), but persisted=false so the
	// caller knows its position was NOT stored and must not advance trackers.
	if persisted, err := st.SaveToken(ctx, "c", "tok0", t0); err != nil || persisted {
		t.Fatalf("stale save: persisted=%v err=%v, want false, nil", persisted, err)
	}
	tok, gotCT, err := st.LoadToken(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok1" || !gotCT.Equal(t1) {
		t.Fatalf("after stale save: token = %q ct = %v, want tok1 %v (rewound!)", tok, gotCT, t1)
	}
	// Newer save still advances.
	if persisted, err := st.SaveToken(ctx, "c", "tok2", t2); err != nil || !persisted {
		t.Fatalf("save tok2: persisted=%v err=%v, want true, nil", persisted, err)
	}
	tok, gotCT, err = st.LoadToken(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok2" || !gotCT.Equal(t2) {
		t.Fatalf("after newer save: token = %q ct = %v, want tok2 %v", tok, gotCT, t2)
	}
	// EQUAL clusterTime, different token: must UPDATE ($lte's equality path).
	// Event clusterTimes carry whole-second granularity, so a train of events
	// committed in the same second all save at the same clusterTime — treating
	// equality as stale would freeze the token at the first of them.
	if persisted, err := st.SaveToken(ctx, "c", "tok3", t2); err != nil || !persisted {
		t.Fatalf("equal-clusterTime save: persisted=%v err=%v, want true, nil", persisted, err)
	}
	tok, gotCT, err = st.LoadToken(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok3" || !gotCT.Equal(t2) {
		t.Fatalf("after equal-clusterTime save: token = %q ct = %v, want tok3 %v", tok, gotCT, t2)
	}
}

// TestSaveTokenSameSecondTokenOrder pins the fine-grained CAS: real
// (KeyString-shaped) resume tokens sharing the same clusterTime SECOND are
// ordered by the token itself — a stale leader saving an EARLIER same-second
// token must be rejected (persisted=false, row unchanged), while a LATER
// same-second token still advances. The coarse cluster_time guard alone
// cannot distinguish these ties.
func TestSaveTokenSameSecondTokenOrder(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx := context.Background()

	const secs = uint32(1752444000)
	ct := time.Unix(int64(secs), 0).UTC()
	mkTok := func(incr uint32) string {
		raw, err := bson.Marshal(bson.D{{Key: "_data", Value: fmt.Sprintf("82%08x%08x", secs, incr)}})
		if err != nil {
			t.Fatalf("marshal token: %v", err)
		}
		return string(raw)
	}
	tokEarly, tokMid, tokLate := mkTok(1), mkTok(2), mkTok(3)

	if persisted, err := st.SaveToken(ctx, "c", tokMid, ct); err != nil || !persisted {
		t.Fatalf("save mid: persisted=%v err=%v, want true, nil", persisted, err)
	}
	// Same second, earlier token: must be classified stale.
	if persisted, err := st.SaveToken(ctx, "c", tokEarly, ct); err != nil || persisted {
		t.Fatalf("same-second stale save: persisted=%v err=%v, want false, nil", persisted, err)
	}
	tok, gotCT, err := st.LoadToken(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if tok != tokMid || !gotCT.Equal(ct) {
		t.Fatalf("after same-second stale save: token changed, want mid kept (early=%v late=%v ct=%v)", tok == tokEarly, tok == tokLate, gotCT)
	}
	// Same second, later token: must advance.
	if persisted, err := st.SaveToken(ctx, "c", tokLate, ct); err != nil || !persisted {
		t.Fatalf("same-second later save: persisted=%v err=%v, want true, nil", persisted, err)
	}
	if tok, _, err = st.LoadToken(ctx, "c"); err != nil || tok != tokLate {
		t.Fatalf("after same-second later save: token not advanced to late (mid=%v early=%v) err=%v", tok == tokMid, tok == tokEarly, err)
	}
	// Idempotent re-save of the stored token is accepted ($lte equality path).
	if persisted, err := st.SaveToken(ctx, "c", tokLate, ct); err != nil || !persisted {
		t.Fatalf("idempotent re-save: persisted=%v err=%v, want true, nil", persisted, err)
	}
}

// TestSaveTokenKeyedSaveRespectsCoarseGuardOnLegacyRow proves the upgrade
// path is not a rewind hole: a row saved WITHOUT token_key (opaque token —
// pre-upgrade rows take the same shape) is guarded by cluster_time, so a
// KeyString-shaped save carrying an OLDER clusterTime is still classified
// stale, while a newer keyed save heals the row onto the fine-grained key.
func TestSaveTokenKeyedSaveRespectsCoarseGuardOnLegacyRow(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx := context.Background()

	const baseSecs = uint32(1752444000)
	base := time.Unix(int64(baseSecs), 0).UTC()
	mkTok := func(secs uint32) string {
		raw, err := bson.Marshal(bson.D{{Key: "_data", Value: fmt.Sprintf("82%08x%08x", secs, uint32(1))}})
		if err != nil {
			t.Fatalf("marshal token: %v", err)
		}
		return string(raw)
	}

	// Opaque token → coarse row without token_key.
	if persisted, err := st.SaveToken(ctx, "c", "opaque-tok", base); err != nil || !persisted {
		t.Fatalf("save opaque: persisted=%v err=%v, want true, nil", persisted, err)
	}
	// Keyed save with OLDER clusterTime must NOT slip in through the
	// missing-token_key hole.
	stale := mkTok(baseSecs - 60)
	if persisted, err := st.SaveToken(ctx, "c", stale, base.Add(-time.Minute)); err != nil || persisted {
		t.Fatalf("stale keyed save over coarse row: persisted=%v err=%v, want false, nil", persisted, err)
	}
	if tok, _, err := st.LoadToken(ctx, "c"); err != nil || tok != "opaque-tok" {
		t.Fatalf("coarse row rewound by stale keyed save: token = %q err=%v, want opaque-tok", tok, err)
	}
	// Newer keyed save heals the row.
	fresh := mkTok(baseSecs + 60)
	if persisted, err := st.SaveToken(ctx, "c", fresh, base.Add(time.Minute)); err != nil || !persisted {
		t.Fatalf("newer keyed save: persisted=%v err=%v, want true, nil", persisted, err)
	}
	if tok, _, err := st.LoadToken(ctx, "c"); err != nil || tok != fresh {
		t.Fatalf("row not healed onto keyed save (still opaque=%v stale=%v) err=%v", tok == "opaque-tok", tok == stale, err)
	}
}

// TestDeleteTokenResetsGroup pins the break-glass reset step: DeleteToken
// erases the stored position so the next LoadToken reports "no token" (the
// relay then starts at "now"), and deleting a missing token is a no-op.
func TestDeleteTokenResetsGroup(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx := context.Background()

	if persisted, err := st.SaveToken(ctx, "c", "tok1", time.Now().UTC()); err != nil || !persisted {
		t.Fatalf("SaveToken: persisted=%v err=%v", persisted, err)
	}
	if err := st.DeleteToken(ctx, "c"); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	tok, _, err := st.LoadToken(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "" {
		t.Fatalf("LoadToken after DeleteToken = %q, want \"\" (position erased)", tok)
	}
	// Deleting a missing token is a no-op, not an error.
	if err := st.DeleteToken(ctx, "c"); err != nil {
		t.Fatalf("DeleteToken on missing row: %v", err)
	}
}

// TestSaveTokenRejectsEmptyToken proves the store defends its own invariant:
// an empty token is never a valid position (LoadToken maps "" to "no stored
// position"), so saving one would erase the group's persisted offset and make
// the next Watch restart "at now", silently skipping events. The save must
// error and leave the stored row untouched.
func TestSaveTokenRejectsEmptyToken(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx := context.Background()

	ct := time.Now().UTC().Truncate(time.Millisecond)
	if persisted, err := st.SaveToken(ctx, "c", "tok1", ct); err != nil || !persisted {
		t.Fatalf("save tok1: persisted=%v err=%v, want true, nil", persisted, err)
	}
	if _, err := st.SaveToken(ctx, "c", "", ct.Add(time.Minute)); err == nil {
		t.Fatal("SaveToken with empty token succeeded; want an error")
	}
	tok, gotCT, err := st.LoadToken(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok1" || !gotCT.Equal(ct) {
		t.Fatalf("stored row changed after rejected empty-token save: token = %q ct = %v, want tok1 %v", tok, gotCT, ct)
	}
}

// TestSaveTokenHealsRowWithoutClusterTime proves a stored row LACKING
// cluster_time (never written by this package — external tampering or manual
// repair) does not stall the group forever: without the $exists guard the
// monotone $lte filter never matches such a row, the upsert hits the _id
// index, and the duplicate-key error is swallowed as "stale" — freezing the
// token silently. The next save must instead update the row in place.
func TestSaveTokenHealsRowWithoutClusterTime(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	ctx := context.Background()

	// Hand-insert a legacy/damaged row via the raw driver: token bytes present,
	// cluster_time absent.
	if _, err := testDB.Collection("outbox_offsets").InsertOne(ctx, bson.M{
		"_id":          "c",
		"resume_token": []byte("legacy"),
	}); err != nil {
		t.Fatalf("hand-insert legacy row: %v", err)
	}

	st := mongodbstore.NewStore(testDB)
	ct := time.Now().UTC().Truncate(time.Millisecond)
	if persisted, err := st.SaveToken(ctx, "c", "tok1", ct); err != nil || !persisted {
		t.Fatalf("save over legacy row: persisted=%v err=%v, want true, nil", persisted, err)
	}
	tok, gotCT, err := st.LoadToken(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok1" || !gotCT.Equal(ct) {
		t.Fatalf("legacy row not healed: token = %q ct = %v, want tok1 %v", tok, gotCT, ct)
	}
}

func TestLeaderLockMutualExclusionAndRelease(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx := context.Background()
	okA, err := st.TryAcquireLeaderLock(ctx, "lock", "A", 30*time.Second)
	if err != nil || !okA {
		t.Fatalf("A acquire = %v, %v; want true", okA, err)
	}
	okB, err := st.TryAcquireLeaderLock(ctx, "lock", "B", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if okB {
		t.Fatal("B acquired while A holds the lock")
	}
	if err := st.ReleaseLeaderLock(ctx, "lock", "A"); err != nil {
		t.Fatal(err)
	}
	okB2, err := st.TryAcquireLeaderLock(ctx, "lock", "B", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !okB2 {
		t.Fatal("B failed to acquire after A released")
	}
}

// TestLeaderLockRenewalSameHolder proves the renewal path: the SAME holder
// re-acquiring an already-live (not expired) lock document must succeed via
// the update branch of TryAcquireLeaderLock, not just the initial-upsert
// branch that TestLeaderLockMutualExclusionAndRelease exercises. Every relay
// tick's TryAcquire call hits exactly this path once leadership is held.
func TestLeaderLockRenewalSameHolder(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx := context.Background()

	okA1, err := st.TryAcquireLeaderLock(ctx, "lock", "A", 30*time.Second)
	if err != nil || !okA1 {
		t.Fatalf("A first acquire = %v, %v; want true", okA1, err)
	}
	// A renews on the still-live document (the update path every relay tick
	// exercises), not a fresh upsert.
	okA2, err := st.TryAcquireLeaderLock(ctx, "lock", "A", 30*time.Second)
	if err != nil || !okA2 {
		t.Fatalf("A renewal = %v, %v; want true", okA2, err)
	}

	// B must still be denied: A's renewal must not have released or handed
	// off the lock.
	okB, err := st.TryAcquireLeaderLock(ctx, "lock", "B", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if okB {
		t.Fatal("B acquired while A holds a renewed lock")
	}
}

// TestLeaderLockExpiredLeaseTakeover proves a standby takes over once the
// lease expires — with the expiry decided by the SERVER clock ($$NOW), the
// same clock that stamped the deadline, so the takeover works regardless of
// skew between relay instances' local clocks.
func TestLeaderLockExpiredLeaseTakeover(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx := context.Background()

	okA, err := st.TryAcquireLeaderLock(ctx, "lock", "A", 200*time.Millisecond)
	if err != nil || !okA {
		t.Fatalf("A acquire = %v, %v; want true", okA, err)
	}
	// B is denied while A's lease is live.
	okB, err := st.TryAcquireLeaderLock(ctx, "lock", "B", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if okB {
		t.Fatal("B acquired while A's lease is still live")
	}
	// Wait out A's lease (comfortably past 200ms), then B must win.
	time.Sleep(700 * time.Millisecond)
	okB2, err := st.TryAcquireLeaderLock(ctx, "lock", "B", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !okB2 {
		t.Fatal("B failed to take over an expired lease")
	}
	// And A, whose lease B replaced, must now be denied.
	okA2, err := st.TryAcquireLeaderLock(ctx, "lock", "A", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if okA2 {
		t.Fatal("A re-acquired while B holds a live lease")
	}
}

// newTestMessage builds a directly-insertable outbox message (bypassing the
// Sender) for the session-binding tests.
func newTestMessage() *outbox.Message {
	md := event.NewMetadata("books.created")
	md.ID = uuid.NewString()
	md.Source = "books-service"
	md.Time = time.Now().UTC()
	return &outbox.Message{ID: md.ID, Metadata: md, Data: []byte("x"), CreateTime: time.Now().UTC()}
}

// TestStoreWithTransactionPublishesAndCommits pins variant B: one call, no
// session ceremony — fn gets a session-bound tx store and the driver's
// session ctx, and a clean return commits the insert.
func TestStoreWithTransactionPublishesAndCommits(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	msg := newTestMessage()

	err := st.WithTransaction(context.Background(), func(ctx context.Context, tx *mongodbstore.Store) error {
		return tx.CreateOutboxMessage(ctx, msg)
	})
	if err != nil {
		t.Fatalf("WithTransaction: %v", err)
	}
	n, err := testDB.Collection("outbox_messages").CountDocuments(context.Background(), bson.M{"_id": msg.ID})
	if err != nil || n != 1 {
		t.Fatalf("count = %d err = %v, want 1 (committed)", n, err)
	}
}

// TestStoreWithTransactionRollsBackOnError pins the abort path: fn's error
// surfaces AND the insert does not commit.
func TestStoreWithTransactionRollsBackOnError(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	msg := newTestMessage()
	boom := errors.New("business write failed")

	err := st.WithTransaction(context.Background(), func(ctx context.Context, tx *mongodbstore.Store) error {
		if err := tx.CreateOutboxMessage(ctx, msg); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithTransaction error = %v, want the callback's own error", err)
	}
	n, err := testDB.Collection("outbox_messages").CountDocuments(context.Background(), bson.M{"_id": msg.ID})
	if err != nil || n != 0 {
		t.Fatalf("count = %d err = %v, want 0 (rolled back)", n, err)
	}
}

// TestWithSessionBindsWithoutMagicCtx pins variant A's whole point: a
// session-bound store joins the transaction even when the caller passes a
// PLAIN ctx — the transaction is a value, not ctx magic.
func TestWithSessionBindsWithoutMagicCtx(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	msg := newTestMessage()

	sess, err := testDB.Client().StartSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.EndSession(context.Background())

	_, err = sess.WithTransaction(context.Background(), func(context.Context) (any, error) {
		// Deliberately NOT the session ctx — the plain Background ctx is the
		// assertion: the bound store must join the transaction on its own.
		return nil, st.WithSession(sess).CreateOutboxMessage(context.Background(), msg) //nolint:contextcheck // plain ctx is the point
	})
	if err != nil {
		t.Fatalf("bound-store publish with plain ctx: %v", err)
	}
	n, err := testDB.Collection("outbox_messages").CountDocuments(context.Background(), bson.M{"_id": msg.ID})
	if err != nil || n != 1 {
		t.Fatalf("count = %d err = %v, want 1 (joined the bound session's txn)", n, err)
	}
}

// TestCreateOutboxMessageBoundSessionWithoutTxnFails pins the constructive
// guard: WithSession outside a running transaction fails loudly, naming the
// fix, instead of writing a phantom row.
func TestCreateOutboxMessageBoundSessionWithoutTxnFails(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)

	sess, err := testDB.Client().StartSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.EndSession(context.Background())

	err = st.WithSession(sess).CreateOutboxMessage(context.Background(), newTestMessage())
	if err == nil || !strings.Contains(err.Error(), "transaction") {
		t.Fatalf("err = %v, want descriptive no-running-transaction error", err)
	}
}

// TestCreateOutboxMessageSessionConflictFails pins the never-pick-silently
// rule: a store bound to one session called with a ctx carrying a DIFFERENT
// session is a caller bug and must error, not choose.
func TestCreateOutboxMessageSessionConflictFails(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)

	sessA, err := testDB.Client().StartSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sessA.EndSession(context.Background())
	sessB, err := testDB.Client().StartSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sessB.EndSession(context.Background())

	ctx := mongo.NewSessionContext(context.Background(), sessA)
	err = st.WithSession(sessB).CreateOutboxMessage(ctx, newTestMessage())
	if err == nil || !strings.Contains(err.Error(), "different session") {
		t.Fatalf("err = %v, want session-conflict rejection", err)
	}
}
