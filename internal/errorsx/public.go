package errorsx

import "errors"

// Public marks reviewed, client-safe prose separately from a private cause.
// Code and message must not contain credentials, raw I/O errors, or other
// internal details. Wrapping preserves the cause for logs and errors.Is/As.
func Public(code, message string, cause error) error {
	return &publicError{code: code, message: message, cause: cause}
}

type publicError struct {
	code, message string
	cause         error
}

func (e *publicError) Error() string {
	if e.cause != nil {
		return e.message + ": " + e.cause.Error()
	}
	return e.message
}
func (e *publicError) Unwrap() error { return e.cause }

// Detailed is an error type whose every value carries reviewed,
// client-safe prose of its own, so none of its values can reach a client
// without one. PublicDetails reads it like a Public wrapper.
type Detailed interface {
	error
	PublicDetails() (code, message string)
}

// PublicDetails extracts only the approved message, never the wrapper/cause.
func PublicDetails(err error) (code, message string, ok bool) {
	var public *publicError
	if errors.As(err, &public) {
		return public.code, public.message, true
	}
	var detailed Detailed
	if errors.As(err, &detailed) {
		code, message = detailed.PublicDetails()
		return code, message, true
	}
	return "", "", false
}
