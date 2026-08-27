module github.com/quarks-tech/protoevent-go/pkg/prometheus

go 1.26.5

require (
	github.com/prometheus/client_golang v1.24.1
	// RELEASE GATE: provisional. This module uses root-module and outbox-module
	// APIs (event.Metadata.Type, relay.Observer) that the pinned tags below
	// already contain (HEAD is the v0.5.0 tag plus pin-only commits), so this
	// pin is not provisional in the same sense as its siblings' — it is pinned
	// to the same v0.5.0 line for consistency and because `check-modules`
	// verifies it builds standalone regardless. See the publishing order in
	// ../transport/outbox/tidb/go.mod if that ever changes.
	github.com/quarks-tech/protoevent-go v0.5.0
	github.com/quarks-tech/protoevent-go/pkg/transport/outbox v0.5.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	golang.org/x/sys v0.47.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
