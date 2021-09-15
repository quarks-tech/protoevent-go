package pubsub

import "context"

type publishInfo struct {
}

type PublishOption interface {
	// before is called before the call is sent to any server.  If before
	// returns a non-nil error, the RPC fails with that error.
	before(*publishInfo) error

	// after is called after the call has completed.  after cannot return an
	// error, so any failures should be reported via output parameters.
	after(*publishInfo)
}

type Publisher interface {
	Publish(ctx context.Context, name string, data interface{}, opts ...PublishOption) error
}
