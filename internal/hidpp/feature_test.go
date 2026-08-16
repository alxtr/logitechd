package hidpp

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestDeviceSessionValidatesDiscoversCallsAndRoutesEvents(t *testing.T) {
	transport := newMemoryTransport()
	session, err := NewSession(transport, SessionOptions{TransactionTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	child, err := NewDeviceSession(session, session, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()

	versionResult := make(chan struct {
		version ProtocolVersion
		err     error
	}, 1)
	go func() {
		version, err := child.Validate(context.Background())
		versionResult <- struct {
			version ProtocolVersion
			err     error
		}{version: version, err: err}
	}()
	request := receiveWrite(t, transport)
	want := []byte{ShortReportID, 2, RootProtocolSubID, RootProtocolCommand, 0, 0, RootPingByte}
	if !bytes.Equal(request, want) {
		t.Fatalf("ping request = %x, want %x", request, want)
	}
	transport.respond(Report{Type: ReportTypeShort, DeviceIndex: 2, SubID: RootProtocolSubID, Function: 1, Parameters: []byte{2, 0, RootPingByte}})
	version := <-versionResult
	if version.err != nil || version.version != (ProtocolVersion{Major: 2, Minor: 0}) {
		t.Fatalf("version = %+v, error=%v", version.version, version.err)
	}

	lookupResult := make(chan struct {
		info FeatureInfo
		err  error
	}, 1)
	go func() {
		info, err := child.LookupFeature(context.Background(), 0x1234)
		lookupResult <- struct {
			info FeatureInfo
			err  error
		}{info: info, err: err}
	}()
	request = receiveWrite(t, transport)
	if request[0] != LongReportID || request[1] != 2 || request[2] != RootFeatureIndex || request[3] != 0 || !bytes.Equal(request[4:6], []byte{0x12, 0x34}) {
		t.Fatalf("feature lookup request = %x", request)
	}
	transport.respond(Report{Type: ReportTypeLong, DeviceIndex: 2, SubID: RootFeatureIndex, Parameters: []byte{7, 0x03, 0x02}})
	lookup := <-lookupResult
	if lookup.err != nil || lookup.info != (FeatureInfo{ID: 0x1234, Index: 7, Type: 3, Version: 2}) {
		t.Fatalf("feature lookup = %+v, error=%v", lookup.info, lookup.err)
	}

	callResult := make(chan struct {
		value []byte
		err   error
	}, 1)
	go func() {
		value, err := child.Call(context.Background(), lookup.info.Index, 0x21, []byte{0xaa, 0xbb})
		callResult <- struct {
			value []byte
			err   error
		}{value: value, err: err}
	}()
	request = receiveWrite(t, transport)
	if request[1] != 2 || request[2] != 7 || request[3] != 0x21 || !bytes.Equal(request[4:6], []byte{0xaa, 0xbb}) {
		t.Fatalf("feature call request = %x", request)
	}
	// A response with the same feature index but a different software ID must
	// not satisfy the waiting call.
	transport.respond(Report{Type: ReportTypeLong, DeviceIndex: 2, SubID: 7, Function: 2, SoftwareID: 0})
	select {
	case result := <-callResult:
		t.Fatalf("call completed on mismatched command: %+v", result)
	case <-time.After(10 * time.Millisecond):
	}
	transport.respond(Report{Type: ReportTypeLong, DeviceIndex: 2, SubID: 7, Function: 2, SoftwareID: 1, Parameters: []byte{0x55}})
	call := <-callResult
	if call.err != nil || len(call.value) == 0 || call.value[0] != 0x55 {
		t.Fatalf("feature call = %x, error=%v", call.value, call.err)
	}

	events := make(chan Report, 1)
	unsub := child.SubscribeEvents(func(report Report) { events <- report })
	defer unsub()
	transport.respond(Report{Type: ReportTypeLong, DeviceIndex: 2, SubID: 7, Function: 0, SoftwareID: 2, Parameters: []byte{1, 2, 3}})
	select {
	case event := <-events:
		if event.DeviceIndex != 2 || event.SubID != 7 || event.SoftwareID != 2 {
			t.Fatalf("event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for unsolicited child event")
	}

	if err := child.CallNoResponse(context.Background(), 7, 0x31, []byte{0x99}); err != nil {
		t.Fatal(err)
	}
	request = receiveWrite(t, transport)
	if request[1] != 2 || request[2] != 7 || request[3] != 0x31 || request[4] != 0x99 {
		t.Fatalf("no-response call = %x", request)
	}
}

func TestDeviceSessionMapsHIDPP20ProtocolErrorsAndUnsupportedValues(t *testing.T) {
	transport := newMemoryTransport()
	session, err := NewDefaultSession(transport)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	child, err := NewDeviceSession(session, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()

	result := make(chan error, 1)
	go func() {
		_, err := child.Call(context.Background(), 9, 0x20, nil)
		result <- err
	}()
	_ = receiveWrite(t, transport)
	transport.respond(Report{Type: ReportTypeLong, DeviceIndex: 3, SubID: FeatureErrorSubID, Function: 0, SoftwareID: 9, Parameters: []byte{0x20, 0x09}})
	protocolErr := <-result
	var typed *ProtocolError
	if !errors.As(protocolErr, &typed) || !errors.Is(protocolErr, ErrProtocol) || typed.RequestSubID != 9 || typed.RequestAddress != 0x20 || typed.Code != 0x09 {
		t.Fatalf("protocol error = %T %+v", protocolErr, protocolErr)
	}

	if _, err := child.Call(context.Background(), 1, 0, make([]byte, longParameterLen+1)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("oversized call error = %v, want unsupported", err)
	}
	if _, err := child.CallWithSoftwareID(context.Background(), 1, 0x10, 0, nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("invalid function error = %v, want unsupported", err)
	}
}

func TestDeviceSessionCallHonorsCancellation(t *testing.T) {
	transport := newMemoryTransport()
	session, err := NewSession(transport, SessionOptions{TransactionTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	child, err := NewDeviceSession(session, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := child.Call(ctx, 3, 0x10, nil)
		result <- err
	}()
	_ = receiveWrite(t, transport)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v, want context.Canceled", err)
	}
}
