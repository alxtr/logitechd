package receiver

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/atremb/logitechd/internal/hidpp"
)

type lifecycleTransport struct {
	reads  chan []byte
	writes chan []byte
	closed chan struct{}
	once   sync.Once
}

func newLifecycleTransport() *lifecycleTransport {
	return &lifecycleTransport{
		reads:  make(chan []byte, 64),
		writes: make(chan []byte, 64),
		closed: make(chan struct{}),
	}
}

func (t *lifecycleTransport) ReadReport(dst []byte) (int, error) {
	select {
	case data := <-t.reads:
		return copy(dst, data), nil
	case <-t.closed:
		return 0, os.ErrClosed
	}
}

func (t *lifecycleTransport) WriteReport(data []byte) error {
	copyData := append([]byte(nil), data...)
	select {
	case t.writes <- copyData:
		return nil
	case <-t.closed:
		return os.ErrClosed
	}
}

func (t *lifecycleTransport) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}

func (t *lifecycleTransport) respond(report hidpp.Report) {
	data, err := hidpp.Build(report)
	if err != nil {
		panic(err)
	}
	t.reads <- data
}

func TestPersistentReceiverSessionEnumeratesLifecycleAndRoutesChildReports(t *testing.T) {
	transport := newLifecycleTransport()
	stopResponder := make(chan struct{})
	go lifecycleResponder(transport, stopResponder)

	events := make(chan ChildEventType, 16)
	ready := make(chan *ChildDevice, 4)
	options := LifecycleOptions{
		Receiver: Options{
			Scanner: ScannerFunc(func() ([]string, error) { return []string{"/dev/hidraw-test"}, nil }),
			Opener: func(path string) (Opened, error) {
				session, err := hidpp.NewSession(transport, hidpp.SessionOptions{TransactionTimeout: 100 * time.Millisecond})
				if err != nil {
					return Opened{}, err
				}
				return Opened{
					Client:   session,
					Reports:  session,
					Metadata: DeviceMetadata{Path: path, ProductID: BoltReceiverProductID},
					Close:    session.Close,
				}, nil
			},
		},
		Callbacks: SessionCallbacks{
			Event: func(event ChildEvent) { events <- event.Type },
			OnChildReady: func(child *ChildDevice) {
				ready <- child
			},
		},
	}
	session, err := OpenSession(context.Background(), options)
	if err != nil {
		close(stopResponder)
		t.Fatal(err)
	}
	defer func() {
		_ = session.Close()
		close(stopResponder)
	}()

	child, ok := session.Child(1)
	if !ok {
		t.Fatal("startup enumeration did not add slot 1")
	}
	metadata := child.Metadata()
	if metadata.WirelessIndex != 1 || metadata.PID != 0x1234 || metadata.DeviceType != DeviceTypeMouse || metadata.Name != "Bolt Mouse" || metadata.ReceiverKind != KindBolt {
		t.Fatalf("child metadata = %+v", metadata)
	}
	if got := receiveLifecycleEvent(t, events); got != ChildAdded {
		t.Fatalf("startup event = %s, want added", got)
	}
	select {
	case readyChild := <-ready:
		if readyChild != child || readyChild.State() != ChildStateReady || readyChild.Metadata().Protocol.Major != 2 {
			t.Fatalf("startup child = %+v state=%s metadata=%+v", readyChild, readyChild.State(), readyChild.Metadata())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for startup child readiness")
	}
	if got := receiveLifecycleEvent(t, events); got != ChildReady {
		t.Fatalf("startup ready event = %s, want ready", got)
	}

	childEvents := make(chan hidpp.Report, 1)
	unsub := child.Client().SubscribeEvents(func(report hidpp.Report) { childEvents <- report })
	defer unsub()
	transport.respond(hidpp.Report{Type: hidpp.ReportTypeLong, DeviceIndex: 1, SubID: 7, Function: 0, Parameters: []byte{0xaa}})
	select {
	case report := <-childEvents:
		if report.DeviceIndex != 1 || report.SubID != 7 || report.Parameters[0] != 0xaa {
			t.Fatalf("child event = %+v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out routing child event")
	}

	transport.respond(hidpp.Report{Type: hidpp.ReportTypeShort, DeviceIndex: 1, SubID: connectionNotification, Parameters: []byte{0x42, 0x34, 0x12}})
	if got := receiveLifecycleEvent(t, events); got != ChildSleeping {
		t.Fatalf("sleep event = %s, want sleeping", got)
	}
	if child.State() != ChildStateSleeping {
		t.Fatalf("child state after sleep = %s", child.State())
	}

	transport.respond(hidpp.Report{Type: hidpp.ReportTypeShort, DeviceIndex: 1, SubID: connectionNotification, Parameters: []byte{0x02, 0x34, 0x12}})
	if got := receiveLifecycleEvent(t, events); got != ChildWoken {
		t.Fatalf("wake event = %s, want woken", got)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for child reinitialization")
	}
	if got := receiveLifecycleEvent(t, events); got != ChildReady {
		t.Fatalf("wake ready event = %s, want ready", got)
	}
	if child.State() != ChildStateReady {
		t.Fatalf("child state after wake = %s", child.State())
	}

	transport.respond(hidpp.Report{Type: hidpp.ReportTypeShort, DeviceIndex: 1, SubID: unpairNotification, Function: 0, SoftwareID: 2})
	if got := receiveLifecycleEvent(t, events); got != ChildRemoved {
		t.Fatalf("remove event = %s, want removed", got)
	}
	if _, ok := session.Child(1); ok {
		t.Fatal("child remains after true disconnect")
	}
}

func lifecycleResponder(transport *lifecycleTransport, stop <-chan struct{}) {
	for {
		select {
		case data := <-transport.writes:
			report, err := hidpp.Parse(data)
			if err != nil {
				return
			}
			respondLifecycleRequest(transport, report)
		case <-stop:
			return
		case <-transport.closed:
			return
		}
	}
}

func respondLifecycleRequest(transport *lifecycleTransport, request hidpp.Report) {
	if request.DeviceIndex != ReceiverDeviceIndex {
		if request.SubID == 0 && request.Type == hidpp.ReportTypeShort && request.CommandByte() == ((hidpp.RootProtocolCommand&0xf0)|hidpp.ClientSoftwareID) {
			transport.respond(hidpp.Report{Type: hidpp.ReportTypeShort, DeviceIndex: request.DeviceIndex, SubID: 0, Function: 1, SoftwareID: request.SoftwareID, Parameters: []byte{2, 0, hidpp.RootPingByte}})
		}
		return
	}
	if request.SubID == hidpp.RegisterGetSubID || request.SubID == hidpp.RegisterLongGetSubID {
		switch request.CommandByte() {
		case byte(ConnectionRegister & 0xff):
			transport.respond(hidpp.Report{Type: request.Type, DeviceIndex: ReceiverDeviceIndex, SubID: request.SubID, Function: request.Function, SoftwareID: request.SoftwareID, Parameters: []byte{1}})
		case byte(BoltUniqueIDRegister & 0xff):
			transport.respond(hidpp.Report{Type: request.Type, DeviceIndex: ReceiverDeviceIndex, SubID: request.SubID, Function: request.Function, SoftwareID: request.SoftwareID, Parameters: []byte{0x55}})
		case byte(ReceiverInfoRegister & 0xff):
			if len(request.Parameters) == 0 {
				return
			}
			selector := request.Parameters[0]
			if selector == boltPairInfoBase+1 {
				transport.respond(hidpp.Report{Type: hidpp.ReportTypeLong, DeviceIndex: ReceiverDeviceIndex, SubID: request.SubID, Function: request.Function, SoftwareID: request.SoftwareID, Parameters: []byte{0, 2, 0x34, 0x12, 0, 0, 0, 0}})
				return
			}
			if selector == boltNameBase+1 {
				params := make([]byte, 16)
				params[2] = 10
				copy(params[3:], "Bolt Mouse")
				transport.respond(hidpp.Report{Type: hidpp.ReportTypeLong, DeviceIndex: ReceiverDeviceIndex, SubID: request.SubID, Function: request.Function, SoftwareID: request.SoftwareID, Parameters: params})
				return
			}
			transport.respond(hidpp.Report{Type: hidpp.ReportTypeLong, DeviceIndex: ReceiverDeviceIndex, SubID: request.SubID, Function: request.Function, SoftwareID: request.SoftwareID, Parameters: make([]byte, 16)})
		}
		return
	}
	if request.SubID == hidpp.RegisterSetSubID {
		transport.respond(hidpp.Report{Type: request.Type, DeviceIndex: ReceiverDeviceIndex, SubID: request.SubID, Function: request.Function, SoftwareID: request.SoftwareID})
	}
}

func receiveLifecycleEvent(t *testing.T, events <-chan ChildEventType) ChildEventType {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for lifecycle event")
		return 0
	}
}
