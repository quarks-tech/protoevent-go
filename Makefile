MODULES := . pkg/transport/outbox pkg/transport/outbox/mongodb pkg/transport/outbox/tidb pkg/transport/rabbitmq

.PHONY: test lint check-modules

# Tests and lint run under go.work, so cross-module changes resolve locally.
test:
	@set -e; for m in $(MODULES); do echo "== $$m"; (cd $$m && go test ./...); done

lint:
	@set -e; for m in $(MODULES); do echo "== $$m"; (cd $$m && golangci-lint run); done

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
