package tidb_test

import (
	"testing"

	tidb "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
)

func TestNewStoreCompiles(t *testing.T) {
	// nil Runner is fine for a construction-only check; no query is issued.
	if tidb.NewStore(nil) == nil {
		t.Fatal("NewStore returned nil")
	}
}

func TestStoreDBConstructs(t *testing.T) {
	if tidb.NewStoreDB(nil) == nil {
		t.Fatal("NewStoreDB returned nil")
	}
}
