module github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb

go 1.26.5

require (
	github.com/go-sql-driver/mysql v1.10.0
	github.com/golang-migrate/migrate/v4 v4.19.1
	github.com/google/uuid v1.6.0
	github.com/moby/moby/api v1.55.0
	// RELEASE GATE: both pins below are PROVISIONAL, and this module is not the only
	// one affected. It uses outbox APIs (Message.Seq, relay/sequence) absent from the
	// pinned outbox v0.4.3, and the branch also adds root-module APIs (event.SplitType,
	// event.Metadata's JSON marshaler) absent from the pinned protoevent-go v0.4.2 —
	// which puts ../mongodb, ../../rabbitmq AND pkg/transport/outbox itself in the same
	// position. Everything builds only inside the repo's go.work; `make check-modules`
	// reproduces what an external `go get` sees.
	//
	// Publishing order — a `replace` cannot substitute, because Go ignores replace
	// directives in a non-main module, so a consumer would still resolve the tags below:
	//  1. merge, then tag the ROOT module vX.Y.Z from master (event.SplitType et al.);
	//  2. bump the protoevent-go pin in pkg/transport/outbox FIRST — it is the module
	//     every store depends on, and its envelope.go is what needs the root's JSON
	//     marshaler — then here, in ../mongodb, and in ../../rabbitmq;
	//  3. tag pkg/transport/outbox/vX.Y.Z;
	//  4. bump the outbox pin here and in ../mongodb, then run `go mod tidy` in BOTH so
	//     go.sum carries the new outbox hashes — a require without them fails the
	//     GOWORK=off build on go.sum verification, which step 5 catches;
	//  5. verify with `make check-modules` — it must report OK for all five modules;
	//  6. only then tag pkg/transport/outbox/tidb, .../mongodb and .../rabbitmq.
	github.com/quarks-tech/protoevent-go v0.4.2
	github.com/quarks-tech/protoevent-go/pkg/transport/outbox v0.4.3
	github.com/testcontainers/testcontainers-go v0.43.0
)

require (
	dario.cat/mergo v1.0.2 // indirect
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/Azure/go-ansiterm v0.0.0-20250102033503-faa5f7b0171c // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/cenkalti/backoff/v4 v4.3.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/containerd/errdefs v1.0.0 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/containerd/log v0.1.0 // indirect
	github.com/containerd/platforms v0.2.1 // indirect
	github.com/cpuguy83/dockercfg v0.3.2 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/docker/go-connections v0.7.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/ebitengine/purego v0.10.1 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/klauspost/compress v1.19.0 // indirect
	github.com/lufia/plan9stats v0.0.0-20260627054121-477a66015f15 // indirect
	github.com/magiconair/properties v1.8.10 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/moby/go-archive v0.2.0 // indirect
	github.com/moby/moby/client v0.5.0 // indirect
	github.com/moby/patternmatcher v0.6.1 // indirect
	github.com/moby/sys/sequential v0.7.0 // indirect
	github.com/moby/sys/user v0.4.1 // indirect
	github.com/moby/sys/userns v0.1.0 // indirect
	github.com/moby/term v0.5.2 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/power-devops/perfstat v0.0.0-20240221224432-82ca36839d55 // indirect
	github.com/shirou/gopsutil/v4 v4.26.6 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	github.com/tklauser/go-sysconf v0.4.0 // indirect
	github.com/tklauser/numcpus v0.12.0 // indirect
	github.com/yusufpapurcu/wmi v1.2.4 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.69.0 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
