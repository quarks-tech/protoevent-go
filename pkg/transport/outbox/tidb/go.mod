module github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb

go 1.25.3

require (
	github.com/go-sql-driver/mysql v1.8.1
	github.com/golang-migrate/migrate/v4 v4.17.1
	github.com/google/uuid v1.6.0
	github.com/quarks-tech/protoevent-go v0.4.2
	github.com/quarks-tech/protoevent-go/pkg/transport/outbox v0.0.0
	github.com/testcontainers/testcontainers-go v0.42.0
)

replace github.com/quarks-tech/protoevent-go/pkg/transport/outbox => ../
