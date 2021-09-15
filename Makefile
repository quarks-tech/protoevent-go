
.PHONY: generate
generate:
	mkdir -p ./gen
	go install ./cmd/protoc-gen-go-pubsub; protoc --go-pubsub_out=./gen ./example/*.proto