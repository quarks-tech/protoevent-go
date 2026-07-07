package mongodb_test

import (
	"testing"

	mongodbstore "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb"
)

func TestNewStoreConstructs(t *testing.T) {
	if mongodbstore.NewStore(nil) == nil {
		t.Fatal("NewStore returned nil")
	}
}

func TestStoreSatisfiesStreamStore(t *testing.T) {
	// Compile-time proof lives in watch.go (var _ stream.StreamStore = ...).
	// This test just ensures the package builds with Watch present.
	_ = mongodbstore.NewStore(nil)
}
