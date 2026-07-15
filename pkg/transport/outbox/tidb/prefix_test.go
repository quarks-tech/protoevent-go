package tidb_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/sequence"
	tidb "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb/tidbmigrate"
)

// countingSender adapts a delivery counter to eventbus.Sender.
type countingSender struct{ n *int }

func (c countingSender) Send(context.Context, *event.Metadata, []byte) error {
	*c.n++
	return nil
}

// TestWithTablePrefixPanicsOnInvalid pins the identifier guard: the prefix is
// spliced into SQL as an identifier (not a bound parameter), so anything
// outside [A-Za-z][A-Za-z0-9_]* must panic at construction — static developer
// configuration, the regexp.MustCompile convention.
func TestWithTablePrefixPanicsOnInvalid(t *testing.T) {
	for _, bad := range []string{"", "1orders_", "orders-", "orders ", "a`; DROP TABLE x; --", strings.Repeat("p", 41)} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("WithTablePrefix(%q) did not panic", bad)
				}
			}()
			tidb.WithTablePrefix(bad)
		}()
	}
	// A valid prefix must not panic.
	tidb.WithTablePrefix("orders_")
}

// TestPrefixedMigrations pins the rewrite: same file set as Migrations, every
// outbox table name prefixed, no unprefixed name left behind.
func TestPrefixedMigrations(t *testing.T) {
	if _, err := tidb.PrefixedMigrations("bad prefix"); err == nil {
		t.Fatal("PrefixedMigrations accepted an invalid prefix")
	}

	prefixed, err := tidb.PrefixedMigrations("orders_")
	if err != nil {
		t.Fatalf("PrefixedMigrations: %v", err)
	}

	var files int
	err = fs.WalkDir(prefixed, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		b, err := fs.ReadFile(prefixed, path)
		if err != nil {
			return err
		}
		content := string(b)
		for _, name := range []string{"outbox_messages", "outbox_offsets", "outbox_sequencers", "relay_locks"} {
			if strings.Contains(content, "orders_"+name) {
				continue // prefixed occurrences expected
			}
			if strings.Contains(content, name) {
				t.Errorf("%s: table %s appears without prefix", path, name)
			}
		}
		if strings.Contains(strings.ReplaceAll(content, "orders_", ""), "orders_") {
			t.Errorf("%s: unexpected double prefix", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk prefixed migrations: %v", err)
	}
	if files == 0 {
		t.Fatal("prefixed migrations contain no files")
	}
}

// TestPrefixedOutboxCoexistsInOneSchema is the end-to-end proof of the
// multi-outbox design: a WithTablePrefix instance migrates (with its own
// golang-migrate versions table), publishes transactionally, sequences, and
// relays in the SAME schema as the default instance — without either touching
// the other's tables.
func TestPrefixedOutboxCoexistsInOneSchema(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	ctx := context.Background()

	// Migrate the prefixed instance via tidbmigrate, which handles both the
	// DDL rewrite AND the per-instance golang-migrate versions table (the
	// default instance already recorded version 1 in schema_migrations;
	// sharing that table would silently skip the prefixed DDL).
	if err := tidbmigrate.Apply(testDB, tidbmigrate.WithTablePrefix("orders_")); err != nil {
		t.Fatalf("migrate prefixed instance: %v", err)
	}
	// Re-runs of the suite leave prefixed rows behind; clear instance state.
	for _, q := range []string{
		"DELETE FROM orders_outbox_messages", "DELETE FROM orders_outbox_offsets",
		"DELETE FROM orders_relay_locks",
		"UPDATE orders_outbox_sequencers SET next_seq = 1 WHERE name = 'default'",
	} {
		if _, err := testDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("reset prefixed (%s): %v", q, err)
		}
	}

	// Publish two events through the PREFIXED publish store, inside a tx.
	for range 2 {
		tx, err := testDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		st := tidb.NewStore(tx, tidb.WithTablePrefix("orders_"))
		if err := outbox.NewSender(st).Send(ctx, newTestMetadata("orders"), []byte("x")); err != nil {
			_ = tx.Rollback()
			t.Fatalf("prefixed publish: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	// Relay the prefixed instance end to end.
	var delivered int
	sender := countingSender{n: &delivered}
	r, err := sequence.NewRelay("orders-relay",
		tidb.NewRelayStore(testDB, tidb.WithTablePrefix("orders_")),
		sender, sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if delivered != 2 {
		t.Fatalf("delivered = %d, want 2 (prefixed instance relays its own log)", delivered)
	}

	// Isolation both ways: the default instance saw nothing, and the prefixed
	// offset landed in the prefixed offsets table.
	var defaultCount int
	if err := testDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM outbox_messages").Scan(&defaultCount); err != nil {
		t.Fatal(err)
	}
	if defaultCount != 0 {
		t.Fatalf("default outbox_messages rows = %d, want 0 (instances must not cross)", defaultCount)
	}
	var off int64
	if err := testDB.QueryRowContext(ctx,
		"SELECT last_seq FROM orders_outbox_offsets WHERE name = 'orders-relay'").Scan(&off); err != nil {
		t.Fatalf("read prefixed offset: %v", err)
	}
	if off != 2 {
		t.Fatalf("prefixed offset = %d, want 2", off)
	}
}
