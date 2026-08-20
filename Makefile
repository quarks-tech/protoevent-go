# PUBLISHED modules: these are what external consumers import, so they are the ones
# check-modules gates under GOWORK=off.
MODULES := . pkg/transport/outbox pkg/transport/outbox/mongodb pkg/transport/outbox/tidb pkg/transport/rabbitmq

# test/e2e is never published and is imported by nothing. It exists only so a test
# can use a store and the RabbitMQ transport together (see its go.mod). It is tested
# and linted, but deliberately NOT gated by check-modules: that gate reproduces what
# an external consumer sees, and nothing consumes this.
TEST_MODULES := $(MODULES) test/e2e

.PHONY: test test-race lint check-modules

# Tests and lint run under go.work, so cross-module changes resolve locally.
test:
	@set -e; for m in $(TEST_MODULES); do echo "== $$m"; (cd $$m && go test ./...); done

# test-race is the concurrency gate. Several tests drive genuinely concurrent
# production code — the sequencer under contending goroutines, the publish-confirm
# map, the gochan buffer — and plain `make test` never asks the detector to look, so
# a data race in that code is invisible to the suite that was written to catch it.
# The explicit -timeout replaces Go's undeclared 10-minute default, which the
# container-backed tests can approach on a cold image pull.
test-race:
	@set -e; for m in $(TEST_MODULES); do echo "== $$m (-race)"; (cd $$m && go test -race -timeout 15m ./...); done

lint:
	@set -e; for m in $(TEST_MODULES); do echo "== $$m"; (cd $$m && golangci-lint run); done

# check-modules is the PRE-RELEASE GATE. go.work makes every submodule resolve
# its siblings from the working tree, which hides stale version pins: a module
# whose go.mod points at a published tag that predates the API it uses builds
# fine here and fails for anyone running `go get`. GOWORK=off reproduces what an
# external consumer sees.
#
# A failure here is not a code bug — it means the dependency tags have not been
# published yet. Follow the publishing order in pkg/transport/outbox/tidb/go.mod:
# tag the dependency module first, bump the pins, then re-run this.
# Reports EVERY failing module, not just the first: the pins are interdependent, so
# stopping at the first failure hides how much of the tree is still ungated.
check-modules:
	@failed=""; for m in $(MODULES); do \
		echo "== $$m (GOWORK=off)"; \
		out=$$(cd $$m && GOWORK=off go build ./... 2>&1) || failed="$$failed $$m"; \
		if [ -n "$$out" ]; then printf '%s\n' "$$out" | sed 's/^/   /'; fi; \
	done; \
	if [ -n "$$failed" ]; then \
		echo; \
		echo "FAIL: not consumable outside go.work:$$failed"; \
		echo "See the RELEASE GATE note in pkg/transport/outbox/tidb/go.mod for the publishing order."; \
		exit 1; \
	fi; \
	echo; echo "OK: every module builds as an external consumer sees it."
