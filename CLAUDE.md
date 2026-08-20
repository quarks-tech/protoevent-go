# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**protoevent-go** is a Go library for building event-driven applications using Protocol Buffers. It provides a publish-subscribe event bus with CloudEvents-compatible metadata, multiple encoding formats (Proto, JSON), and extensible transport mechanisms.

## Modules

The repo is a **six-module `go.work` workspace**, not a single module. `go test ./...`
at the root reaches only the root module — it does NOT compile or test the other
five, where most of the outbox and RabbitMQ code lives:

| Path | Module | Published |
| --- | --- | --- |
| `.` | `github.com/quarks-tech/protoevent-go` | yes |
| `pkg/transport/outbox` | `.../pkg/transport/outbox` | yes |
| `pkg/transport/outbox/tidb` | `.../pkg/transport/outbox/tidb` | yes |
| `pkg/transport/outbox/mongodb` | `.../pkg/transport/outbox/mongodb` | yes |
| `pkg/transport/rabbitmq` | `.../pkg/transport/rabbitmq` | yes |
| `test/e2e` | `.../test/e2e` | **never** |

`test/e2e` exists only so one test can import a STORE and the RabbitMQ TRANSPORT
together — no published module may depend on both. It is tested and linted but
excluded from `check-modules`, and its `replace` directives are legitimate
precisely because nothing consumes it.

Each published module is tagged separately (`pkg/transport/outbox/v0.4.3`, …), so a
submodule's `go.mod` can pin a published version of its parent that predates the
working tree. `go.work` hides that; `make check-modules` is what surfaces it.

**Release choreography.** A dependency must be TAGGED before a dependent can pin
it, and `replace` cannot substitute — Go ignores replace directives in a non-main
module, so a consumer still resolves the literal `require`. Releases therefore go
in waves, each pin bump its own commit: root → {outbox, rabbitmq} → {tidb,
mongodb}. Run `go mod tidy` with **`GOWORK=off`** during this; with the workspace
active, tidy resolves siblings from the tree and never writes their hashes into
`go.sum`, so the build passes locally and fails for everyone else. The RELEASE
GATE comments in `pkg/transport/outbox/tidb/go.mod` carry the full order and are
deleted once the pins are real. Never move a published tag — `proxy.golang.org`
caches permanently; fix forward with a new patch plus a `retract`.

## Common Commands

```bash
# Run every module's tests (root-only `go test ./...` misses five modules)
make test

# The concurrency gate. Several tests drive genuinely concurrent production code
# (the sequencer under contention, the publish-confirm map, the gochan buffer),
# and plain `make test` never asks the detector to look.
make test-race

# Lint every module
make lint

# PRE-RELEASE GATE: build each module with GOWORK=off, i.e. as an external
# consumer sees it. Catches a submodule whose go.mod pins a parent version older
# than the API it uses — which builds fine under go.work and fails on `go get`.
# EXPECTED TO FAIL between a breaking change and the release that tags it.
make check-modules

# Throughput/latency harnesses are opt-in — minutes long, Docker-backed.
OUTBOX_MEASURE=1 go test ./test/e2e -run TestThroughput -v -timeout 20m
```

Container-backed tests skip when Docker is unavailable and hard-fail under `CI`.

## Architecture

### Core Packages

**`pkg/eventbus/`** - Core pub-sub implementation
- `Publisher` is an interface (implemented by `PublisherImpl`); publish options
  cover content type, source, subject, schema, extensions
- `Subscriber` is a **struct**, not an interface — `NewSubscriber(name)` then
  `RegisterHandler`, and `Subscribe(ctx, receiver)` to run
- Transport seams are `Sender` (publish side) and `Receiver` (subscribe side),
  with `Setuper` for topology declaration and `Processor` for one decoded
  delivery. `BatchSender` is `Sender`'s optional capability, overlapping
  per-event acknowledgements
- `PublisherInterceptor` and `SubscriberInterceptor` for middleware chains
- Automatic metadata completion (ID, timestamp, source)

**`pkg/event/`** - CloudEvents metadata model
- `Metadata` struct following CloudEvents 1.0 specification, with its own JSON marshaler
- Content type parsing for codec selection; `SplitType` is the one definition of
  the `<service>.<event>` shape, shared by the publish guard and every transport's routing
- `ErrUnsendable` marks metadata a transport can never serialize — the marker that
  lets a relay park such a row instead of wedging on it forever

**`pkg/encoding/`** - Pluggable codec system
- `Codec` interface (Name, Marshal, Unmarshal) with global registry
- Built-in codecs: `proto/` (protobuf, default) and `json/` (protojson with DiscardUnknown)
- Registered subtypes are exactly `proto` and `json`, so `application/protobuf`
  resolves to NO codec — the spelling most CloudEvents stacks use is wrong here

**`pkg/transport/gochan/`** - In-memory Go channel transport
- `SendReceiver` combining sender and receiver interfaces
- Default channel buffer depth: 20

**`pkg/transport/rabbitmq/`** (own module) - AMQP transport over `quarks-tech/amqpx`
- `Sender` (publisher confirms on by default; `SendBatch` pipelines a page's
  confirms) and `Receiver`; `parkinglot/` is the alternative receiver whose retry
  wait is served broker-side by a queue TTL
- `message/contentmode/{binary,structured}` implement the two CloudEvents AMQP bindings
- `internal/consume` and `internal/publish` hold what the two receivers and the two
  publishers share — each existed as diverged copies before
- Deliveries are handled ONE AT A TIME on a single goroutine: prefetch buys
  buffering, never concurrency. Any per-delivery sleep stalls the whole consumer

**`pkg/transport/outbox/`** (own module) - Transactional outbox
- Publish side (`Sender`, `Store`, `PublisherFactory`) is tx-scoped so the event
  row commits with the business write
- Two relay runtimes: `relay/sequence` (TiDB sequenced log) and `relay/stream`
  (MongoDB change-stream tail). `internal/lane` holds the per-message delivery
  policy BOTH share — it was written twice and drifted, and a drift there is a
  delivery bug rather than a compile error
- Store backends are separate modules: `tidb/` and `mongodb/`

### Code Generation

The protoc plugin (protoc-gen-go-eventbus) was removed from this repo (commit 85349b1); there is no cmd/ directory. Generated `.pb.eventbus.go` files provide EventPublisher interfaces, handlers, and registration functions for messages marked `(quarks_tech.protoevent.v1.enabled) = true`.

### Design Patterns

- **Functional Options:** PublishOption, PublisherOption, SubscriberOption for
  configuration. Relay option structs are UNEXPORTED, so the `With*` constructors
  are the single validation surface
- **Interface Segregation:** Small interfaces (Sender, Receiver, Setuper, Processor)
- **Optional capabilities, discovered by type assertion:** the pervasive idiom
  here. A store or sender advertises extra ability by implementing an interface
  (`eventbus.BatchSender`, `sequence.FencedCommitter`, `sequence.Clock`,
  `sequence.Sweeper`, `stream.IndexEnsurer`); absence degrades gracefully rather
  than failing. The exception is leadership — a store that cannot elect is a
  construction error unless waived, because silently missing it means duplicate
  delivery from every replica
- **Interceptor Chain:** Middleware for cross-cutting concerns on both publisher and subscriber sides

## Conventions

- **Modern Go is enforced, not remembered.** `modernize` is in `.golangci.yml`;
  golangci bundles a lagging x/tools, so on a toolchain bump also run
  `go run golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest ./...`
  per module. Use `errors.AsType[T]` (never `errors.As`), `sync.WaitGroup.Go`
  (never `Add`/`Done`), `for b.Loop()` in benchmarks
- **`testing/synctest` for anything timing-based.** Long waits (a 15-minute stuck
  threshold, an hour-long sweep interval, a backoff ladder) collapse to an exact,
  instant assertion inside a bubble. Assert `elapsed == want`, not `elapsed < fuzz`
- `context.Background()` in tests is deliberate (`usetesting.context-background`
  is off): TestMain bootstrap, synctest bubbles where `t.Context` would escape,
  and the store suites' convention. Note `t.Context()` is cancelled BEFORE
  `t.Cleanup` runs
- **Mutation-test every fix.** Break the code, watch the new test fail with the
  right message, restore. A test that cannot fail is worthless
- **Measure before claiming.** Findings in this repo carry numbers, and several
  plausible hypotheses have collapsed under measurement

### Test harnesses

Real dependencies, not mocks. Each store/transport module ships a testcontainers
harness — `tidbtest.Start`, `mongodbtest.Start` (plus `StartCluster` for a
3-node replica set with real elections, and `WithArbiter()` for the PSA topology
where losing a node fails SILENTLY), `rabbitmqtest.Start` (whose `Instance`
reaches the broker as an operator: `Rabbitmqctl`, `StopApp`/`StartApp`, alarm and
`max_message_size` setters, `DeclareQueue` with arguments). All return
`ErrDockerUnavailable`, which callers skip on locally and hard-fail on under `CI`.

Long, Docker-backed measurement suites are gated behind `OUTBOX_MEASURE=1`
(`test/e2e/throughput_test.go`, `pkg/transport/rabbitmq/measure_test.go`).
Regression guards derived from them are deliberately NOT gated — a bound worth
having is a bound worth checking every run.

## Dependencies

Root module: exactly two — `google.golang.org/protobuf` (Protocol Buffers runtime)
and `github.com/google/uuid` (event IDs). Keep it that way; the root is what every
other module pins.

`github.com/quarks-tech/protoevent` (the proto-definitions repo, no `-go` suffix)
is **not** a Go dependency of anything here — it is not in any `go.mod` and not
imported by any file. It supplies the `.proto` options a CONSUMER's definitions
import (`quarks_tech.protoevent.v1.enabled`), which the removed protoc plugin read
at generation time. An earlier version of this document listed it as a dependency;
it never was.

Each transport module carries its own: `quarks-tech/amqpx` + `rabbitmq/amqp091-go`
(rabbitmq), `go-sql-driver/mysql` + `golang-migrate` (tidb), `mongo-driver/v2`
(mongodb), `testcontainers-go` in the store modules for their integration suites.
Keeping them apart is deliberate — an outbox consumer must not pull amqp091.

## Example Usage

See the worked examples in the root README.md (proto definition, publisher,
subscriber over gochan/RabbitMQ, receiver defaults, `SendBatch`) and
pkg/transport/outbox/README.md (outbox publish + TiDB/MongoDB relay wiring).
There is no runnable example/ directory. The relay examples are executed by
`relay/stream/readme_example_test.go` — a documented option set that does not
construct is a test failure, because one shipped that way once.
