package hidpp

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

type memoryTransport struct {
	reads  chan []byte
	writes chan []byte
	closed chan struct{}
	once   sync.Once
}

func newMemoryTransport() *memoryTransport {
	return &memoryTransport{
		reads:  make(chan []byte, 16),
		writes: make(chan []byte, 16),
		closed: make(chan struct{}),
	}
}

func (t *memoryTransport) ReadReport(dst []byte) (int, error) {
	select {
	case data := <-t.reads:
		if len(data) > len(dst) {
			return len(data), nil
		}
		return copy(dst, data), nil
	case <-t.closed:
		return 0, os.ErrClosed
	}
}

func (t *memoryTransport) WriteReport(data []byte) error {
	copyData := append([]byte(nil), data...)
	select {
	case t.writes <- copyData:
		return nil
	case <-t.closed:
		return os.ErrClosed
	}
}

func (t *memoryTransport) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}

func (t *memoryTransport) respond(report Report) {
	data, err := Build(report)
	if err != nil {
		panic(err)
	}
	t.reads <- data
}

func TestGetRegisterBuildsShortRequestAndMatchesResponse(t *testing.T) {
	transport := newMemoryTransport()
	session, err := NewDefaultSession(transport)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	type result struct {
		value []byte
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		value, err := session.GetRegister(context.Background(), 0x02, 0x1a, 2)
		resultCh <- result{value: value, err: err}
	}()

	request := receiveWrite(t, transport)
	wantRequest := []byte{ShortReportID, 0x02, RegisterGetSubID, 0x1a, 0, 0, 0}
	if !bytes.Equal(request, wantRequest) {
		t.Fatalf("request = %x, want %x", request, wantRequest)
	}
	transport.respond(Report{
		Type:        ReportTypeShort,
		DeviceIndex: 0x02,
		SubID:       RegisterGetSubID,
		Function:    0x01,
		SoftwareID:  0x0a,
		Parameters:  []byte{0xde, 0xad},
	})

	got := <-resultCh
	if got.err != nil {
		t.Fatal(got.err)
	}
	if !bytes.Equal(got.value, []byte{0xde, 0xad}) {
		t.Fatalf("value = %x, want dead", got.value)
	}
}

func TestSetRegisterUsesLongRequestAndNoResponseSetUsesShortRequest(t *testing.T) {
	transport := newMemoryTransport()
	session, err := NewDefaultSession(transport)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	setResult := make(chan error, 1)
	go func() {
		setResult <- session.SetRegister(context.Background(), 0x01, 0xa5, []byte{1, 2, 3, 4})
	}()
	request := receiveWrite(t, transport)
	want := append([]byte{LongReportID, 0x01, RegisterSetSubID, 0xa5}, []byte{1, 2, 3, 4}...)
	want = append(want, make([]byte, longParameterLen-4)...)
	if !bytes.Equal(request, want) {
		t.Fatalf("long set request = %x, want %x", request, want)
	}
	transport.respond(Report{
		Type:        ReportTypeLong,
		DeviceIndex: 0x01,
		SubID:       RegisterSetSubID,
		Function:    0x0a,
		SoftwareID:  0x05,
	})
	if err := <-setResult; err != nil {
		t.Fatal(err)
	}

	if err := session.SetRegisterNoResponse(context.Background(), 0x01, 0xa5, []byte{9, 8}); err != nil {
		t.Fatal(err)
	}
	request = receiveWrite(t, transport)
	want = []byte{ShortReportID, 0x01, RegisterSetSubID, 0xa5, 9, 8, 0}
	if !bytes.Equal(request, want) {
		t.Fatalf("no-response request = %x, want %x", request, want)
	}
}

func TestSessionConvertsHIDPPErrorReport(t *testing.T) {
	transport := newMemoryTransport()
	session, err := NewDefaultSession(transport)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	resultCh := make(chan error, 1)
	go func() {
		_, err := session.GetRegister(context.Background(), 0x03, 0x22, 1)
		resultCh <- err
	}()
	_ = receiveWrite(t, transport)
	transport.respond(Report{
		Type:        ReportTypeShort,
		DeviceIndex: 0x03,
		SubID:       RegisterErrorSubID,
		Parameters:  []byte{RegisterGetSubID, 0x22, 0x02},
	})

	got := <-resultCh
	var protocolErr *ProtocolError
	if !errors.As(got, &protocolErr) {
		t.Fatalf("error = %T %v, want ProtocolError", got, got)
	}
	if !errors.Is(got, ErrProtocol) || protocolErr.Code != 0x02 || protocolErr.RequestAddress != 0x22 {
		t.Fatalf("protocol error = %+v", protocolErr)
	}
}

func TestLongRequestMatchesShortHIDPPErrorReport(t *testing.T) {
	transport := newMemoryTransport()
	session, err := NewDefaultSession(transport)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	resultCh := make(chan error, 1)
	go func() {
		resultCh <- session.SetRegister(context.Background(), 0x04, 0x33, []byte{1, 2, 3, 4})
	}()
	request := receiveWrite(t, transport)
	if request[0] != LongReportID {
		t.Fatalf("request report ID = 0x%02x, want long", request[0])
	}
	transport.respond(Report{
		Type:        ReportTypeShort,
		DeviceIndex: 0x04,
		SubID:       RegisterErrorSubID,
		Parameters:  []byte{RegisterSetSubID, 0x33, 0x02},
	})

	got := <-resultCh
	var protocolErr *ProtocolError
	if !errors.As(got, &protocolErr) || protocolErr.Code != 0x02 {
		t.Fatalf("error = %T %v, want long-request ProtocolError", got, got)
	}
}

func TestLongRegisterAddressUsesLongSubIDs(t *testing.T) {
	transport := newMemoryTransport()
	session, err := NewDefaultSession(transport)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	getResult := make(chan struct {
		value []byte
		err   error
	}, 1)
	go func() {
		value, err := session.GetRegister(context.Background(), 0x01, 0x2b3, 1)
		getResult <- struct {
			value []byte
			err   error
		}{value: value, err: err}
	}()
	request := receiveWrite(t, transport)
	if request[0] != LongReportID || request[2] != RegisterLongGetSubID || request[3] != 0xb3 {
		t.Fatalf("long get request = %x", request)
	}
	transport.respond(Report{
		Type:        ReportTypeLong,
		DeviceIndex: 0x01,
		SubID:       RegisterLongGetSubID,
		Function:    0x0b,
		SoftwareID:  0x03,
		Parameters:  []byte{0x42},
	})
	get := <-getResult
	if get.err != nil || !bytes.Equal(get.value, []byte{0x42}) {
		t.Fatalf("long get result = %x, %v", get.value, get.err)
	}

	setResult := make(chan error, 1)
	go func() {
		setResult <- session.SetRegister(context.Background(), 0x01, 0x2c1, []byte{7})
	}()
	request = receiveWrite(t, transport)
	if request[0] != LongReportID || request[2] != RegisterLongSetSubID || request[3] != 0xc1 || request[4] != 7 {
		t.Fatalf("long set request = %x", request)
	}
	transport.respond(Report{Type: ReportTypeLong, DeviceIndex: 0x01, SubID: RegisterLongSetSubID, Function: 0x0c, SoftwareID: 0x01})
	if err := <-setResult; err != nil {
		t.Fatal(err)
	}
}

func TestLongRegisterReadCarriesSubregisterSelector(t *testing.T) {
	transport := newMemoryTransport()
	session, err := NewDefaultSession(transport)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	resultCh := make(chan struct {
		value []byte
		err   error
	}, 1)
	go func() {
		value, err := session.GetRegisterWithParameters(context.Background(), 0xff, 0x2b5, 7, 0x22)
		resultCh <- struct {
			value []byte
			err   error
		}{value: value, err: err}
	}()

	request := receiveWrite(t, transport)
	wantPrefix := []byte{LongReportID, 0xff, RegisterLongGetSubID, 0xb5, 0x22}
	if !bytes.Equal(request[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("long subregister request = %x, want prefix %x", request, wantPrefix)
	}
	transport.respond(Report{
		Type:        ReportTypeLong,
		DeviceIndex: 0xff,
		SubID:       RegisterLongGetSubID,
		Function:    0x0b,
		SoftwareID:  0x05,
		Parameters:  []byte{1, 2, 3, 4, 5, 6, 7},
	})
	result := <-resultCh
	if result.err != nil || !bytes.Equal(result.value, []byte{1, 2, 3, 4, 5, 6, 7}) {
		t.Fatalf("long subregister result = %x, %v", result.value, result.err)
	}
}

func TestSessionReportsMalformedResponse(t *testing.T) {
	transport := newMemoryTransport()
	session, err := NewDefaultSession(transport)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	resultCh := make(chan error, 1)
	go func() {
		_, err := session.GetRegister(context.Background(), 0x01, 0x01, 1)
		resultCh <- err
	}()
	_ = receiveWrite(t, transport)
	transport.reads <- []byte{ShortReportID, 0x01}

	got := <-resultCh
	var malformed *MalformedResponseError
	if !errors.As(got, &malformed) || !errors.Is(got, ErrMalformedResponse) {
		t.Fatalf("error = %T %v, want MalformedResponseError", got, got)
	}
}

func TestSessionTimeoutAndCancellation(t *testing.T) {
	transport := newMemoryTransport()
	session, err := NewSession(transport, SessionOptions{TransactionTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	_, err = session.GetRegister(context.Background(), 0x01, 0x01, 1)
	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) || !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout error = %T %v", err, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() {
		_, err := session.GetRegister(ctx, 0x01, 0x02, 1)
		resultCh <- err
	}()
	_ = receiveWrite(t, transport)
	cancel()
	if err := <-resultCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v, want context.Canceled", err)
	}
}

func TestRegisterOperationsRejectInvalidPayloadSizes(t *testing.T) {
	transport := newMemoryTransport()
	session, err := NewDefaultSession(transport)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	for _, size := range []int{0, -1, longParameterLen + 1} {
		if _, err := session.GetRegister(context.Background(), 0x01, 0x01, size); !errors.Is(err, ErrUnsupported) {
			t.Errorf("GetRegister size %d error = %v, want ErrUnsupported", size, err)
		}
	}
	for _, address := range []uint16{0x100, 0x300} {
		if _, err := session.GetRegister(context.Background(), 0x01, address, 1); !errors.Is(err, ErrUnsupported) {
			t.Errorf("GetRegister address 0x%04x error = %v, want ErrUnsupported", address, err)
		}
	}
	for _, value := range [][]byte{nil, make([]byte, longParameterLen+1)} {
		if err := session.SetRegister(context.Background(), 0x01, 0x01, value); !errors.Is(err, ErrUnsupported) {
			t.Errorf("SetRegister len %d error = %v, want ErrUnsupported", len(value), err)
		}
		if err := session.SetRegisterNoResponse(context.Background(), 0x01, 0x01, value); !errors.Is(err, ErrUnsupported) {
			t.Errorf("SetRegisterNoResponse len %d error = %v, want ErrUnsupported", len(value), err)
		}
	}
}

func TestSessionCloseUnblocksTransaction(t *testing.T) {
	transport := newMemoryTransport()
	session, err := NewDefaultSession(transport)
	if err != nil {
		t.Fatal(err)
	}

	resultCh := make(chan error, 1)
	go func() {
		_, err := session.GetRegister(context.Background(), 0x01, 0x01, 1)
		resultCh <- err
	}()
	_ = receiveWrite(t, transport)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	var closedErr *ClosedTransportError
	if err := <-resultCh; !errors.As(err, &closedErr) || !errors.Is(err, ErrClosedTransport) {
		t.Fatalf("transaction error = %T %v, want ClosedTransportError", err, err)
	}
}

func TestSessionDispatchesConcurrentCallersByIdentity(t *testing.T) {
	transport := newMemoryTransport()
	session, err := NewDefaultSession(transport)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	type result struct {
		device byte
		value  []byte
		err    error
	}
	results := make(chan result, 2)
	go func() {
		value, err := session.GetRegister(context.Background(), 0x01, 0x10, 1)
		results <- result{device: 0x01, value: value, err: err}
	}()
	go func() {
		value, err := session.GetRegister(context.Background(), 0x02, 0x20, 1)
		results <- result{device: 0x02, value: value, err: err}
	}()

	first := receiveWrite(t, transport)
	second := receiveWrite(t, transport)
	for _, request := range [][]byte{first, second} {
		if request[2] != RegisterGetSubID {
			t.Fatalf("unexpected concurrent request %x", request)
		}
	}
	respond := func(request []byte) {
		value := byte(0xaa)
		if request[3] == 0x20 {
			value = 0xbb
		}
		transport.respond(Report{Type: ReportTypeShort, DeviceIndex: request[1], SubID: RegisterGetSubID, Function: request[3] >> 4, SoftwareID: request[3] & 0x0f, Parameters: []byte{value}})
	}
	respond(second)
	respond(first)

	seen := make(map[byte][]byte)
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		seen[result.device] = result.value
	}
	if !bytes.Equal(seen[0x01], []byte{0xaa}) || !bytes.Equal(seen[0x02], []byte{0xbb}) {
		t.Fatalf("concurrent results = %x, want device 01=aa device 02=bb", seen)
	}
}

func receiveWrite(t *testing.T, transport *memoryTransport) []byte {
	t.Helper()
	select {
	case data := <-transport.writes:
		return data
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for transport write")
		return nil
	}
}
