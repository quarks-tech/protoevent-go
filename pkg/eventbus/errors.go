package eventbus

import "errors"

type UnprocessableEventError struct {
	err error
}

func NewUnprocessableEventError(err error) *UnprocessableEventError {
	return &UnprocessableEventError{err: err}
}

func (e *UnprocessableEventError) Error() string { return "unprocessable event: " + e.err.Error() }

func (e *UnprocessableEventError) Unwrap() error { return e.err }

func IsUnprocessableEventError(err error) bool {
	var unprocessableEventError *UnprocessableEventError

	return errors.As(err, &unprocessableEventError)
}
