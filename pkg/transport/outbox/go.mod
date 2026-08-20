module github.com/quarks-tech/protoevent-go/pkg/transport/outbox

go 1.26.5

require (
	github.com/google/uuid v1.6.0
	// RELEASE GATE: provisional. envelope.go depends on root-module APIs added on
	// this branch (event.Metadata's JSON marshaler, event.SplitType) that the
	// pinned v0.4.2 tag does not contain, so it builds only inside the repo's
	// go.work. This module is the FIRST of the four to need the bump — it is the
	// one every store module depends on — and it was long missing from the
	// publishing order in ./tidb/go.mod for that reason. Tag the root module
	// first, then bump this pin, then tag this module; verify with
	// `make check-modules`.
	github.com/quarks-tech/protoevent-go v0.5.0
)

require google.golang.org/protobuf v1.36.11
