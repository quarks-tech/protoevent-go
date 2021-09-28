package connpool

import (
	"sync/atomic"
	"time"

	"github.com/streadway/amqp"
)

type Conn struct {
	amqpConn    *amqp.Connection
	amqpChannel *amqp.Channel
	pooled      bool
	createdAt   time.Time
	usedAt      int64 // atomic
}

func NewConn(amqpConn *amqp.Connection, amqpCh *amqp.Channel) *Conn {
	conn := &Conn{
		amqpConn:    amqpConn,
		amqpChannel: amqpCh,
		createdAt:   time.Now(),
	}

	conn.SetUsedAt(time.Now())

	return conn
}

func (c *Conn) UsedAt() time.Time {
	return time.Unix(atomic.LoadInt64(&c.usedAt), 0)
}

func (c *Conn) SetUsedAt(tm time.Time) {
	atomic.StoreInt64(&c.usedAt, tm.Unix())
}

func (c *Conn) Channel() *amqp.Channel {
	c.SetUsedAt(time.Now())

	return c.amqpChannel
}

func (c *Conn) NotifyClose(receiver chan *amqp.Error) chan *amqp.Error {
	c.SetUsedAt(time.Now())

	return c.amqpConn.NotifyClose(receiver)
}

func (c *Conn) IsClosed() bool {
	c.SetUsedAt(time.Now())

	return c.amqpConn.IsClosed()
}

func (c *Conn) Close() error {
	return c.amqpConn.Close()
}
