package tidb_test

import (
	"testing"
	"testing/fstest"

	tidb "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
)

func TestMigrationsEmbedded(t *testing.T) {
	if err := fstest.TestFS(tidb.Migrations, "migrations/000001_create_outbox.up.sql"); err != nil {
		t.Fatalf("migration not embedded: %v", err)
	}
}
