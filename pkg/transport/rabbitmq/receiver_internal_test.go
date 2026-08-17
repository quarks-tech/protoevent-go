package rabbitmq

import "testing"

// recordingLogger is a Logger that is not DefaultLogger, so the tests below can
// tell "the option was applied" from "the default survived".
type recordingLogger struct{ msgs []string }

func (l *recordingLogger) Errorf(format string, _ ...any) { l.msgs = append(l.msgs, format) }

// TestDefaultReceiverLoggerIsNotNil pins the one thing the default has to be.
// It used to be nil, and the only place the receiver logs through it is
// consume.Spec.Logger, which nil-guards — so an event that would not unmarshal
// was rejected (dropped, or dead-lettered under WithDLX) with no error to the
// caller and no log line anywhere to find it by.
func TestDefaultReceiverLoggerIsNotNil(t *testing.T) {
	if defaultReceiverOptions().logger == nil {
		t.Fatal("default logger is nil: an undecodable delivery would be rejected silently")
	}
}

func TestWithLoggerNilIsNoop(t *testing.T) {
	o := defaultReceiverOptions()
	l := &recordingLogger{}
	WithLogger(l)(&o)
	WithLogger(nil)(&o)

	if o.logger != Logger(l) {
		t.Fatal("WithLogger(nil) replaced a previously set logger")
	}
}

// TestWithLoggerNilKeepsTheDefault covers the other order: WithLogger(nil) on a
// fresh option set must not strip the default back to silence.
func TestWithLoggerNilKeepsTheDefault(t *testing.T) {
	o := defaultReceiverOptions()
	WithLogger(nil)(&o)

	if o.logger == nil {
		t.Fatal("WithLogger(nil) cleared the default logger")
	}
}
