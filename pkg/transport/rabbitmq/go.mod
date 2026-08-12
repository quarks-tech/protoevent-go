module github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq

go 1.26.5

require (
	github.com/json-iterator/go v1.1.12
	github.com/quarks-tech/amqpx v0.3.5
	// RELEASE GATE: provisional. This module uses root-module APIs added on this
	// branch (event.SplitType, event.Metadata's JSON marshaler) that the pinned
	// v0.4.2 tag does not contain, so it builds only inside the repo's go.work.
	// Tag the root module first, then bump this pin — see the publishing order in
	// pkg/transport/outbox/tidb/go.mod, and verify with `make check-modules`.
	github.com/quarks-tech/protoevent-go v0.4.2
	github.com/rabbitmq/amqp091-go v1.13.0
	github.com/rs/xid v1.6.0
)

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
