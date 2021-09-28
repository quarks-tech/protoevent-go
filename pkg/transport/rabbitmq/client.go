package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync/atomic"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/connpool"
	"github.com/streadway/amqp"
)

type Command func(ctx context.Context, conn *connpool.Conn) error

type Client struct {
	config   *Config
	connPool *connpool.ConnPool
}

func NewClient(config *Config) *Client {
	return &Client{
		config:   config,
		connPool: newConnPool(config),
	}
}

func (c *Client) getConn(ctx context.Context) (*connpool.Conn, error) {
	if c.config.Limiter != nil {
		err := c.config.Limiter.Allow()
		if err != nil {
			return nil, err
		}
	}

	conn, err := c.connPool.Get(ctx)
	if err != nil {
		if c.config.Limiter != nil {
			c.config.Limiter.ReportResult(err)
		}
		return nil, err
	}

	return conn, nil
}

func (c *Client) releaseConn(ctx context.Context, conn *connpool.Conn, err error) {
	if c.config.Limiter != nil {
		c.config.Limiter.ReportResult(err)
	}

	if isBadConnErr(err, false) {
		c.connPool.Remove(ctx, conn, err)
	} else {
		c.connPool.Put(ctx, conn)
	}
}

func (c *Client) withConn(ctx context.Context, fn Command) error {
	conn, err := c.getConn(ctx)
	if err != nil {
		return err
	}

	defer func() {
		c.releaseConn(ctx, conn, err)
	}()

	done := ctx.Done() //nolint:ifshort

	if done == nil {
		err = fn(ctx, conn)
		return err
	}

	errc := make(chan error, 1)
	go func() {
		errc <- fn(ctx, conn)
	}()

	select {
	case <-done:
		_ = conn.Close()
		// Wait for the goroutine to finish and send something.
		<-errc

		err = ctx.Err()
		return err
	case err = <-errc:
		return err
	}
}

func (c *Client) Process(ctx context.Context, cmd Command) error {
	var lastErr error

	for attempt := 0; attempt <= c.config.MaxRetries; attempt++ {
		retry, err := c.doProcess(ctx, cmd, attempt)
		if err == nil || !retry {
			return err
		}

		lastErr = err
	}

	return lastErr
}

func (c *Client) doProcess(ctx context.Context, cmd Command, attempt int) (bool, error) {
	if attempt > 0 {
		if err := sleepWithContext(ctx, c.retryBackoff(attempt)); err != nil {
			return false, err
		}
	}

	retryTimeout := uint32(1)

	err := c.withConn(ctx, cmd)
	if err == nil {
		return false, nil
	}

	retry := shouldRetry(err, atomic.LoadUint32(&retryTimeout) == 1)
	return retry, err
}

func (c *Client) retryBackoff(attempt int) time.Duration {
	return retryBackoff(attempt, c.config.MinRetryBackoff, c.config.MaxRetryBackoff)
}

func (c *Client) Close() error {
	return c.connPool.Close()
}

func retryBackoff(retry int, minBackoff, maxBackoff time.Duration) time.Duration {
	if retry < 0 {
		panic("not reached")
	}
	if minBackoff == 0 {
		return 0
	}

	d := minBackoff << uint(retry)
	if d < minBackoff {
		return maxBackoff
	}

	d = minBackoff + time.Duration(rand.Int63n(int64(d)))

	if d > maxBackoff || d < minBackoff {
		d = maxBackoff
	}

	return d
}

func sleepWithContext(ctx context.Context, dur time.Duration) error {
	t := time.NewTimer(dur)
	defer t.Stop()

	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func isBadConnErr(err error, allowTimeout bool) bool {
	if err != nil {
		fmt.Printf("1 %s", err)
	}

	switch err {
	case nil:
		return false
	case context.Canceled, context.DeadlineExceeded:
		return true
	}

	var amqpErr amqp.Error

	if errors.As(err, &amqpErr) {
		switch amqpErr.Code {
		case amqp.ConnectionForced, amqp.ChannelError:
			return true
		}
	}

	if allowTimeout {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return !netErr.Temporary()
		}
	}

	return true
}

func shouldRetry(err error, retryTimeout bool) bool {
	if err != nil {
		fmt.Printf("2 %s", err)
	}

	switch err {
	case io.EOF, io.ErrUnexpectedEOF:
		return true
	case nil, context.Canceled, context.DeadlineExceeded:
		return false
	}

	if v, ok := err.(timeoutError); ok {
		if v.Timeout() {
			return retryTimeout
		}
		return true
	}

	var amqpErr amqp.Error

	if errors.As(err, &amqpErr) {
		switch amqpErr.Code {
		case amqp.ConnectionForced, amqp.ChannelError, amqp.InternalError:
			return true
		}
	}

	return false
}

type timeoutError interface {
	Timeout() bool
}
