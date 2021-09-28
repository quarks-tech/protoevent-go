
.PHONY: generate
generate:
	mkdir -p ./example/gen
	go install ./cmd/protoc-gen-go-eventbus
	protoc --go_out=./example/gen ./example/*.proto
	protoc --go-eventbus_out=./example/gen ./example/*.proto

.PHONY: rabbitmq
rabbitmq:
	docker run -d --hostname my-rabbit --name some-rabbit -p 8080:15672 -p 5672:5672 rabbitmq:3-management