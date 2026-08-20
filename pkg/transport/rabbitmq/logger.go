package rabbitmq

import (
	"fmt"
	"log/slog"
)

type Logger interface {
	Errorf(format string, args ...any)
}

// DefaultLogger is what a receiver logs through when WithLogger is not given.
//
// It is deliberately not nil. Every failure a receiver reports through this
// interface is one where the delivery is already being disposed of — an event
// that would not unmarshal is rejected, a park that could not be acked keeps its
// QoS slot — so a nil logger turned each of those into a silent drop, with no
// error returned to the caller and no log anywhere to find it by. Silence is
// available on request (pass a Logger that discards), but it is the wrong
// default for a consumer that is throwing messages away.
//
// slog.Default() is read per call, not captured once, so a logger installed
// after NewReceiver still applies.
func DefaultLogger() Logger { return slogLogger{} }

type slogLogger struct{}

func (slogLogger) Errorf(format string, args ...any) {
	slog.Default().Error(fmt.Sprintf(format, args...))
}
