package rabbitmq

import (
	"context"
	"strings"
	"testing"
)

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

// TestSetupRejectsDLXWithoutTopologySetup pins that an option which cannot take
// effect fails loudly instead of reporting success.
//
// WithDLX() alone was silently inert: Setup returns early when setupTopology is
// false, BEFORE the block that declares the dead-letter exchange and sets
// x-dead-letter-exchange on the queue. So a caller who added WithDLX precisely
// because rejected deliveries were disappearing got no .dlx exchange, no queue
// argument, and no error — and the option they reached for was their evidence the
// problem was fixed.
//
// It cannot be honored silently either: x-dead-letter-exchange is an argument of
// queue.declare, so only whoever declares the queue can set it.
//
// A nil client is safe here: the guard must fire before any broker call, which is
// itself part of the contract.
func TestSetupRejectsDLXWithoutTopologySetup(t *testing.T) {
	r := NewReceiver(nil, WithDLX())

	err := r.Setup(context.Background(), "consumer")
	if err == nil {
		t.Fatal("Setup with WithDLX() and no WithTopologySetup() = nil; the option declares no " +
			"dead-letter exchange and changes nothing, so reporting success tells the caller " +
			"their rejected deliveries are now safe when they are still being discarded")
	}
	if !strings.Contains(err.Error(), "WithTopologySetup") {
		t.Fatalf("Setup error = %v, want it to name WithTopologySetup as the remedy", err)
	}

	// The remedy must actually be accepted (it reaches the client, so only the
	// validation is exercised here — a nil client would panic past this point).
	r2 := NewReceiver(nil, WithDLX(), WithTopologySetup())
	if r2.options.enableDLX != true || r2.options.setupTopology != true {
		t.Fatal("WithDLX()+WithTopologySetup() did not set both flags")
	}
}
