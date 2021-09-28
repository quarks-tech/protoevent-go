package rabbitmq

import (
	"runtime"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/connpool"
	"github.com/streadway/amqp"
)

type Limiter interface {
	// Allow returns nil if operation is allowed or an error otherwise.
	// If operation is allowed client must ReportResult of the operation
	// whether it is a success or a failure.
	Allow() error
	// ReportResult reports the result of the previously allowed operation.
	// nil indicates a success, non-nil error usually indicates a failure.
	ReportResult(result error)
}

type Config struct {
	Address string

	AMQP amqp.Config

	// Maximum number of retries before giving up.
	// Default is 3 retries; -1 (not 0) disables retries.
	MaxRetries int
	// Minimum backoff between each retry.
	// Default is 8 milliseconds; -1 disables backoff.
	MinRetryBackoff time.Duration
	// Maximum backoff between each retry.
	// Default is 512 milliseconds; -1 disables backoff.
	MaxRetryBackoff time.Duration

	// Dial timeout for establishing new connections.
	// Default is 5 seconds.
	DialTimeout time.Duration

	// Type of connection pool.
	// true for FIFO pool, false for LIFO pool.
	// Note that fifo has higher overhead compared to lifo.
	PoolFIFO bool
	// Maximum number of socket connections.
	// Default is 10 connections per every available CPU as reported by runtime.GOMAXPROCS.
	PoolSize int
	// Minimum number of idle connections which is useful when establishing
	// new connection is slow.
	MinIdleConns int
	// Connection age at which client retires (closes) the connection.
	// Default is to not close aged connections.
	MaxConnAge time.Duration
	// Amount of time client waits for connection if all connections
	// are busy before returning an error.
	// Default is ReadTimeout + 1 second.
	PoolTimeout time.Duration
	// Amount of time after which client closes idle connections.
	// Should be less than server's timeout.
	// Default is 5 minutes. -1 disables idle timeout check.
	IdleTimeout time.Duration
	// Frequency of idle checks made by idle connections reaper.
	// Default is 1 minute. -1 disables idle connections reaper,
	// but idle connections are still discarded by the client
	// if IdleTimeout is set.
	IdleCheckFrequency time.Duration

	// Limiter interface used to implemented circuit breaker or rate limiter.
	Limiter Limiter
}

func (c *Config) complete() {
	if c.DialTimeout == 0 {
		c.DialTimeout = 5 * time.Second
	}

	if c.AMQP.Dial == nil {
		c.AMQP.Dial = amqp.DefaultDial(c.DialTimeout)
	}

	if c.PoolSize == 0 {
		c.PoolSize = 10 * runtime.GOMAXPROCS(0)
	}

	if c.PoolTimeout == 0 {
		c.PoolTimeout = time.Second
	}
	if c.IdleTimeout == 0 {
		//c.IdleTimeout = 5 * time.Minute
	}
	if c.IdleCheckFrequency == 0 {
		c.IdleCheckFrequency = time.Minute
	}

	if c.MaxRetries == -1 {
		c.MaxRetries = 0
	} else if c.MaxRetries == 0 {
		c.MaxRetries = 3
	}
	switch c.MinRetryBackoff {
	case -1:
		c.MinRetryBackoff = 0
	case 0:
		c.MinRetryBackoff = 8 * time.Millisecond
	}
	switch c.MaxRetryBackoff {
	case -1:
		c.MaxRetryBackoff = 0
	case 0:
		c.MaxRetryBackoff = 512 * time.Millisecond
	}
}

func newConnPool(cfg *Config) *connpool.ConnPool {
	cfg.complete()

	return connpool.New(&connpool.Options{
		Dialer: func() (*amqp.Connection, *amqp.Channel, error) {
			conn, err := amqp.DialConfig("amqp://"+cfg.Address, cfg.AMQP)
			if err != nil {
				return nil, nil, err
			}

			ch, err := conn.Channel()
			if err != nil {
				_ = conn.Close()
				return nil, nil, err
			}

			return conn, ch, nil
		},
		PoolFIFO:           cfg.PoolFIFO,
		PoolSize:           cfg.PoolSize,
		MinIdleConns:       cfg.MinIdleConns,
		MaxConnAge:         cfg.MaxConnAge,
		PoolTimeout:        cfg.PoolTimeout,
		IdleTimeout:        cfg.IdleTimeout,
		IdleCheckFrequency: cfg.IdleCheckFrequency,
	})
}
