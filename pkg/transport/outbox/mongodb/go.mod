module github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb

go 1.25.3

require (
	github.com/quarks-tech/protoevent-go v0.4.2
	github.com/quarks-tech/protoevent-go/pkg/transport/outbox v0.0.0
	github.com/testcontainers/testcontainers-go v0.42.0
	github.com/testcontainers/testcontainers-go/modules/mongodb v0.42.0
	go.mongodb.org/mongo-driver/v2 v2.5.1
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.18.5 // indirect
	github.com/xdg-go/pbkdf2 v1.0.0 // indirect
	github.com/xdg-go/scram v1.2.0 // indirect
	github.com/xdg-go/stringprep v1.0.4 // indirect
	github.com/youmark/pkcs8 v0.0.0-20240726163527-a2c0da244d78 // indirect
	golang.org/x/crypto v0.48.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/text v0.34.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/quarks-tech/protoevent-go/pkg/transport/outbox => ../
