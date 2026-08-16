package receiver

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/atremb/logitechd/internal/hidpp"
)

type registerKey struct {
	address uint16
	params  string
}

type fakeRegisterClient struct {
	mu        sync.Mutex
	gets      []registerKey
	responses map[registerKey][]byte
	errors    map[registerKey]error
	setCalls  []setCall
	setError  error
}

type setCall struct {
	address uint16
	value   []byte
}

func (f *fakeRegisterClient) GetRegister(ctx context.Context, _ byte, address uint16, size int) ([]byte, error) {
	return f.getRegister(ctx, address, size)
}

func (f *fakeRegisterClient) GetRegisterWithParameters(ctx context.Context, _ byte, address uint16, size int, params ...byte) ([]byte, error) {
	return f.getRegister(ctx, address, size, params...)
}

func (f *fakeRegisterClient) getRegister(_ context.Context, address uint16, _ int, params ...byte) ([]byte, error) {
	key := registerKey{address: address, params: string(params)}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, key)
	if err := f.errors[key]; err != nil {
		return nil, err
	}
	value, ok := f.responses[key]
	if !ok {
		return nil, fmt.Errorf("no fake response for %#v", key)
	}
	return append([]byte(nil), value...), nil
}

func (f *fakeRegisterClient) SetRegister(_ context.Context, _ byte, address uint16, value []byte) error {
	f.mu.Lock()
	f.setCalls = append(f.setCalls, setCall{address: address, value: append([]byte(nil), value...)})
	f.mu.Unlock()
	return f.setError
}

type fakeReportSource struct {
	mu      sync.Mutex
	handler func(hidpp.Report)
}

func (f *fakeReportSource) SetReportHandler(handler func(hidpp.Report)) {
	f.mu.Lock()
	f.handler = handler
	f.mu.Unlock()
}

func (f *fakeReportSource) emit(report hidpp.Report) {
	f.mu.Lock()
	handler := f.handler
	f.mu.Unlock()
	if handler != nil {
		handler(report)
	}
}

func TestProbeRecognizesBoltWithoutProductID(t *testing.T) {
	client := &fakeRegisterClient{
		responses: map[registerKey][]byte{
			{address: ConnectionRegister}:   {1},
			{address: BoltUniqueIDRegister}: {0x55},
		},
		errors: map[registerKey]error{
			{address: ReceiverInfoRegister, params: string([]byte{0x03})}: errors.New("not unifying"),
		},
	}
	r, err := New(Opened{Client: client, Metadata: DeviceMetadata{ProductID: 0xc999}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	kind, err := r.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if kind != KindBolt || r.Kind() != KindBolt {
		t.Fatalf("kind = %v, receiver kind = %v; want Bolt", kind, r.Kind())
	}
}

func TestProbeRecognizesUnifyingAndEnumeratesSlots(t *testing.T) {
	client := unifyingClient()
	r, err := New(Opened{Client: client, Metadata: DeviceMetadata{ProductID: 0x1234}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if kind, err := r.Probe(context.Background()); err != nil || kind != KindUnifying {
		t.Fatalf("Probe() = %v, %v; want Unifying", kind, err)
	}
	devices, err := r.EnumeratePaired(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []PairedDevice{{Slot: 2, PID: 0x1234, DeviceType: DeviceTypeMouse, Name: "MX Master 3S"}}
	if !reflect.DeepEqual(devices, want) {
		t.Fatalf("devices = %+v, want %+v", devices, want)
	}
	selected, ok := FindPairedDevice(devices, "MX Master 3S", 2)
	if !ok || selected.Slot != 2 || selected.PID != 0x1234 {
		t.Fatalf("selected = %+v, ok=%v; want slot 2 MX Master 3S", selected, ok)
	}
}

func TestBoltPairingAndNameLayouts(t *testing.T) {
	client := &fakeRegisterClient{
		responses: map[registerKey][]byte{
			{address: ReceiverInfoRegister, params: string([]byte{boltPairInfoBase + 2})}: {0, 2, 0x34, 0x12, 0xaa, 0xbb, 0xcc, 0xdd},
			{address: ReceiverInfoRegister, params: string([]byte{boltNameBase + 2, 1})}:  nameResponse(2, "Bolt Mouse"),
		},
	}
	r, err := New(Opened{Client: client})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.kind = KindBolt

	device, present, err := r.readPairedDevice(context.Background(), KindBolt, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !present || device.PID != 0x1234 || device.DeviceType != DeviceTypeMouse || device.Name != "Bolt Mouse" {
		t.Fatalf("device = %+v, present=%v", device, present)
	}
}

func TestMalformedReceiverResponsesAreRejected(t *testing.T) {
	if _, _, err := decodePairingInfo(KindUnifying, 1, []byte{1, 2, 3}); !errors.Is(err, hidpp.ErrMalformedResponse) {
		t.Fatalf("short pairing response error = %v, want malformed", err)
	}
	if _, err := decodeName([]byte{0, 15}, 1, 2); !errors.Is(err, hidpp.ErrMalformedResponse) {
		t.Fatalf("oversized name error = %v, want malformed", err)
	}
	if _, err := ParseDeviceEvent(hidpp.Report{DeviceIndex: 2, SubID: connectionNotification, Parameters: []byte{0x02}}); !errors.Is(err, hidpp.ErrMalformedResponse) {
		t.Fatalf("short notification error = %v, want malformed", err)
	}
	if err := decodeProbeInfo([]byte{1, 2}); !errors.Is(err, hidpp.ErrMalformedResponse) {
		t.Fatalf("short probe response error = %v, want malformed", err)
	}
}

func TestConnectionAndUnpairNotificationDecoding(t *testing.T) {
	connected, err := ParseDeviceEvent(hidpp.Report{
		DeviceIndex: 2,
		SubID:       connectionNotification,
		Function:    1,
		SoftwareID:  0,
		Parameters:  []byte{0x22, 0x34, 0x12},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !connected.Connected || !connected.Paired || !connected.Encrypted || connected.PID != 0x1234 || connected.DeviceType != DeviceTypeMouse {
		t.Fatalf("connected event = %+v", connected)
	}

	disconnected, err := ParseDeviceEvent(hidpp.Report{
		DeviceIndex: 2,
		SubID:       connectionNotification,
		Function:    1,
		Parameters:  []byte{0x62, 0x34, 0x12},
	})
	if err != nil {
		t.Fatal(err)
	}
	if disconnected.Connected || !disconnected.Paired {
		t.Fatalf("disconnected event = %+v", disconnected)
	}

	unpaired, err := ParseDeviceEvent(hidpp.Report{DeviceIndex: 2, SubID: unpairNotification, Function: 0, SoftwareID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if unpaired.Connected || unpaired.Paired || unpaired.HasPID {
		t.Fatalf("unpaired event = %+v", unpaired)
	}
	paddedUnpaired, err := ParseDeviceEvent(hidpp.Report{DeviceIndex: 2, SubID: unpairNotification, Function: 0, SoftwareID: 2, Parameters: []byte{0, 0, 0}})
	if err != nil || paddedUnpaired.HasPID {
		t.Fatalf("padded unpaired event = %+v, error=%v", paddedUnpaired, err)
	}
}

func TestEventHandlerUsesInjectedReportSourceAndConfigureWrites(t *testing.T) {
	client := &fakeRegisterClient{responses: map[registerKey][]byte{
		{address: ConnectionRegister}:                                 {1},
		{address: ReceiverInfoRegister, params: string([]byte{0x03})}: {0, 0, 0, 0, 0, 0, 6},
	}}
	source := &fakeReportSource{}
	r, err := New(Opened{Client: client, Reports: source})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	var got DeviceEvent
	var gotErr error
	if err := r.SetEventHandler(func(event DeviceEvent, err error) { got, gotErr = event, err }); err != nil {
		t.Fatal(err)
	}
	if err := r.Configure(context.Background()); err != nil {
		t.Fatal(err)
	}
	source.emit(hidpp.Report{DeviceIndex: 2, SubID: connectionNotification, SoftwareID: 0x00, Parameters: []byte{0x02, 0x34, 0x12}})
	if gotErr != nil || got.Slot != 2 || !got.Connected {
		t.Fatalf("handler event = %+v, error=%v", got, gotErr)
	}
	if len(client.setCalls) != 2 || !reflect.DeepEqual(client.setCalls[0].value, []byte{0, 9, 0}) || !reflect.DeepEqual(client.setCalls[1].value, []byte{2}) {
		t.Fatalf("setup writes = %+v", client.setCalls)
	}
}

func TestDiscoverAndOpenAndEnumerateUseInjectedScannerAndClose(t *testing.T) {
	client := unifyingClient()
	var openedPath string
	var closeCount int
	snapshot, err := OpenAndEnumerate(context.Background(), Options{
		Scanner: ScannerFunc(func() ([]string, error) { return []string{"/dev/hidraw9"}, nil }),
		Opener: func(path string) (Opened, error) {
			openedPath = path
			return Opened{Client: client, Metadata: DeviceMetadata{ProductID: 0x9999}, Close: func() error { closeCount++; return nil }}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if openedPath != "/dev/hidraw9" || closeCount != 1 || snapshot.Kind != KindUnifying || len(snapshot.Devices) != 1 {
		t.Fatalf("path=%q closes=%d snapshot=%+v", openedPath, closeCount, snapshot)
	}
}

func unifyingClient() *fakeRegisterClient {
	responses := map[registerKey][]byte{
		{address: ConnectionRegister}:                                 {1},
		{address: ReceiverInfoRegister, params: string([]byte{0x03})}: {0, 0, 0, 0, 0, 0, 6},
	}
	for slot := byte(1); slot <= maxReceiverSlots; slot++ {
		responses[registerKey{address: ReceiverInfoRegister, params: string([]byte{unifyingPairInfoBase + slot - 1})}] = make([]byte, 8)
	}
	responses[registerKey{address: ReceiverInfoRegister, params: string([]byte{unifyingPairInfoBase + 1})}] = []byte{0, 0, 0, 0x12, 0x34, 0, 0, 0x02}
	responses[registerKey{address: ReceiverInfoRegister, params: string([]byte{unifyingNameBase + 1})}] = nameResponse(1, "MX Master 3S")
	return &fakeRegisterClient{responses: responses}
}

func nameResponse(lengthOffset int, name string) []byte {
	data := make([]byte, 16)
	textOffset := lengthOffset + 1
	data[lengthOffset] = byte(len(name))
	copy(data[textOffset:], name)
	return data
}
