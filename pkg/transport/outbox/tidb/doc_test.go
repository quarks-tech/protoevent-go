package tidb_test

import (
	"testing"
	"testing/fstest"

	tidb "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
)

func TestMigrationsEmbedded(t *testing.T) {
	// List every migration file: losing any one from the embed keeps the
	// build green but breaks tidbmigrate.Apply for fresh installs.
	if err := fstest.TestFS(tidb.Migrations,
		"migrations/000001_create_outbox.up.sql",
		"migrations/000001_create_outbox.down.sql",
		"migrations/000002_add_create_time_index.up.sql",
		"migrations/000002_add_create_time_index.down.sql",
	); err != nil {
		t.Fatalf("migration not embedded: %v", err)
	}
}
