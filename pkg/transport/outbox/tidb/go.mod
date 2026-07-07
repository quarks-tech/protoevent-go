module github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb

go 1.25.3

require (
	github.com/google/uuid v1.6.0
	github.com/quarks-tech/protoevent-go v0.4.2
	github.com/quarks-tech/protoevent-go/pkg/transport/outbox v0.0.0
)

require google.golang.org/protobuf v1.36.11 // indirect

replace github.com/quarks-tech/protoevent-go/pkg/transport/outbox => ../
