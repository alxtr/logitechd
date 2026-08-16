package hidpp

import (
	"errors"
	"fmt"
)

var (
	// ErrTimeout identifies a transaction that did not receive a response in
	// time.
	ErrTimeout = errors.New("hidpp: transaction timeout")
	// ErrMalformedResponse identifies a report that cannot be decoded or an
	// otherwise incomplete protocol response.
	ErrMalformedResponse = errors.New("hidpp: malformed response")
	// ErrProtocol identifies a HID++ protocol error returned by the device.
	ErrProtocol = errors.New("hidpp: protocol error")
	// ErrClosedTransport identifies a session whose transport is no longer
	// usable.
	ErrClosedTransport = errors.New("hidpp: closed transport")
	// ErrUnsupported identifies an operation that this package cannot encode or
	// perform.
	ErrUnsupported = errors.New("hidpp: unsupported operation")
)

// TimeoutError reports the operation that exceeded its context or session
// deadline.
type TimeoutError struct {
	Operation string
	Cause     error
}

func (e *TimeoutError) Error() string {
	if e == nil {
		return ErrTimeout.Error()
	}
	if e.Operation == "" {
		return ErrTimeout.Error()
	}
	return fmt.Sprintf("hidpp: %s timed out", e.Operation)
}

func (e *TimeoutError) Unwrap() error {
	if e == nil || e.Cause == nil {
		return ErrTimeout
	}
	return errors.Join(ErrTimeout, e.Cause)
}

// Is lets callers use errors.Is with ErrTimeout even when the underlying
// context error is also retained.
func (e *TimeoutError) Is(target error) bool {
	return target == ErrTimeout || (e != nil && e.Cause != nil && errors.Is(e.Cause, target))
}

// MalformedResponseError reports a raw report that could not be used as a
// response. Data is a copy of the bytes received from the transport.
type MalformedResponseError struct {
	Data  []byte
	Cause error
}

func (e *MalformedResponseError) Error() string {
	if e == nil {
		return ErrMalformedResponse.Error()
	}
	if e.Cause == nil {
		return fmt.Sprintf("hidpp: malformed response %x", e.Data)
	}
	return fmt.Sprintf("hidpp: malformed response %x: %v", e.Data, e.Cause)
}

func (e *MalformedResponseError) Unwrap() error {
	if e == nil {
		return ErrMalformedResponse
	}
	return e.Cause
}

func (e *MalformedResponseError) Is(target error) bool {
	return target == ErrMalformedResponse || (e != nil && e.Cause != nil && errors.Is(e.Cause, target))
}

// ProtocolError is the HID++ 1.0 error report associated with a request.
// Code is the device's error code; RequestSubID and RequestAddress identify
// the operation that failed.
type ProtocolError struct {
	DeviceIndex    byte
	Code           byte
	RequestSubID   byte
	RequestAddress byte
	Parameters     []byte
}

func (e *ProtocolError) Error() string {
	if e == nil {
		return ErrProtocol.Error()
	}
	return fmt.Sprintf("hidpp: protocol error 0x%02x for sub-ID 0x%02x address 0x%02x", e.Code, e.RequestSubID, e.RequestAddress)
}

func (e *ProtocolError) Unwrap() error {
	return ErrProtocol
}

// FeatureIndex returns the HID++ 2.0 feature index associated with the error.
// For a HID++ 1.0 error this is the original request sub-ID as well, which
// keeps callers independent of the wire dialect.
func (e *ProtocolError) FeatureIndex() byte {
	if e == nil {
		return 0
	}
	return e.RequestSubID
}

// ClosedTransportError reports a read or close event that made the session
// unusable. Cause retains the transport's original error when available.
type ClosedTransportError struct {
	Cause error
}

func (e *ClosedTransportError) Error() string {
	if e == nil || e.Cause == nil {
		return ErrClosedTransport.Error()
	}
	return fmt.Sprintf("hidpp: transport closed: %v", e.Cause)
}

func (e *ClosedTransportError) Unwrap() error {
	if e == nil || e.Cause == nil {
		return ErrClosedTransport
	}
	return errors.Join(ErrClosedTransport, e.Cause)
}

func (e *ClosedTransportError) Is(target error) bool {
	return target == ErrClosedTransport || (e != nil && e.Cause != nil && errors.Is(e.Cause, target))
}

// UnsupportedError describes an operation outside this package's protocol
// surface or wire-size limits.
type UnsupportedError struct {
	Operation string
	Detail    string
}

func (e *UnsupportedError) Error() string {
	if e == nil {
		return ErrUnsupported.Error()
	}
	if e.Detail == "" {
		return fmt.Sprintf("hidpp: unsupported %s", e.Operation)
	}
	return fmt.Sprintf("hidpp: unsupported %s: %s", e.Operation, e.Detail)
}

func (e *UnsupportedError) Unwrap() error {
	return ErrUnsupported
}

func malformedResponse(data []byte, cause error) error {
	copyData := append([]byte(nil), data...)
	return &MalformedResponseError{Data: copyData, Cause: cause}
}
