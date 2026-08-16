package receiver

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/atremb/logitechd/internal/hidpp"
)

// ChildState is the manager-visible lifecycle state of a wireless child.
type ChildState uint8

const (
	ChildStateAdded ChildState = iota + 1
	ChildStateReady
	ChildStateSleeping
	ChildStateRemoved
)

func (s ChildState) String() string {
	switch s {
	case ChildStateAdded:
		return "added"
	case ChildStateReady:
		return "ready"
	case ChildStateSleeping:
		return "sleeping"
	case ChildStateRemoved:
		return "removed"
	default:
		return "unknown"
	}
}

// ChildMetadata combines receiver pairing metadata with the information
// needed to select a future device-specific implementation. WirelessIndex is
// the HID++ device index and is stable for the lifetime of a receiver slot.
type ChildMetadata struct {
	WirelessIndex byte
	PID           uint16
	DeviceType    DeviceType
	Name          string
	ReceiverKind  Kind
	ReceiverPath  string
	Protocol      hidpp.ProtocolVersion
}

// ChildEventType identifies a lifecycle callback.
type ChildEventType uint8

const (
	ChildAdded ChildEventType = iota + 1
	ChildReady
	ChildSleeping
	ChildWoken
	ChildRemoved
)

func (t ChildEventType) String() string {
	switch t {
	case ChildAdded:
		return "child-added"
	case ChildReady:
		return "child-ready"
	case ChildSleeping:
		return "child-sleeping"
	case ChildWoken:
		return "child-woken"
	case ChildRemoved:
		return "child-removed"
	default:
		return "unknown"
	}
}

// ChildEvent is the common event form for users that prefer one callback.
type ChildEvent struct {
	Type     ChildEventType
	Child    *ChildDevice
	Metadata ChildMetadata
}

// SessionCallbacks exposes child lifecycle changes. Callbacks are invoked by
// the lifecycle worker and should return promptly; they must not perform a
// blocking operation on the receiver session.
type SessionCallbacks struct {
	Event           func(ChildEvent)
	OnChildAdded    func(*ChildDevice)
	OnChildReady    func(*ChildDevice)
	OnChildSleeping func(*ChildDevice)
	OnChildWoken    func(*ChildDevice)
	OnChildRemoved  func(*ChildDevice)
}

// LifecycleOptions controls OpenSession. Receiver selects and opens one
// receiver using the Phase 3 discovery options. ChildTimeout only overrides
// the context used for root validation; zero leaves the HID++ session timeout
// in charge.
type LifecycleOptions struct {
	Receiver     Options
	Callbacks    SessionCallbacks
	ChildTimeout time.Duration
}

// ChildDevice is a safe handle for one wireless index. Its HID++ client uses
// the same physical reader and transport as the receiver session.
type ChildDevice struct {
	stateMu sync.RWMutex
	state   ChildState
	meta    ChildMetadata
	client  *hidpp.DeviceSession
	initing bool
}

// Metadata returns a copy of the current child metadata.
func (d *ChildDevice) Metadata() ChildMetadata {
	if d == nil {
		return ChildMetadata{}
	}
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.meta
}

// State returns the current lifecycle state.
func (d *ChildDevice) State() ChildState {
	if d == nil {
		return ChildStateRemoved
	}
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.state
}

// Client returns the HID++ 2.0 client. The client remains usable while the
// child is sleeping, but requests may time out until a wake event arrives.
func (d *ChildDevice) Client() *hidpp.DeviceSession {
	if d == nil {
		return nil
	}
	return d.client
}

// ReceiverSession is a persistent owner of one opened receiver and all known
// child sessions. One worker consumes receiver notifications; all wire
// transactions continue through the single HID++ Session reader.
type ReceiverSession struct {
	receiver *Receiver
	client   hidpp.ExchangeClient

	callbacks    SessionCallbacks
	childTimeout time.Duration
	baseCtx      context.Context
	cancel       context.CancelFunc

	stateMu  sync.RWMutex
	children map[byte]*ChildDevice
	closed   bool

	reports    chan hidpp.Report
	init       chan *ChildDevice
	done       chan struct{}
	workerDone chan struct{}

	closeOnce  sync.Once
	closeError error
}

// OpenSession selects the first supported receiver, configures notifications,
// enumerates paired slots, and leaves the receiver open until Close. It
// requires the opened client to be a hidpp.Session-compatible exchange client
// so child HID++ 2.0 requests share the receiver's transaction dispatcher.
func OpenSession(ctx context.Context, options LifecycleOptions) (*ReceiverSession, error) {
	if ctx == nil {
		return nil, errors.New("receiver: nil lifecycle context")
	}
	receivers, err := Discover(ctx, options.Receiver)
	if err != nil {
		return nil, err
	}
	if len(receivers) == 0 {
		return nil, ErrNoReceiver
	}
	selected := receivers[0]
	for _, other := range receivers[1:] {
		_ = other.Close()
	}
	client, ok := selected.client.(hidpp.ExchangeClient)
	if !ok {
		_ = selected.Close()
		return nil, &hidpp.UnsupportedError{
			Operation: "receiver child session",
			Detail:    "opened client does not provide HID++ transaction exchange",
		}
	}
	if selected.reports == nil {
		_ = selected.Close()
		return nil, errors.New("receiver: opened receiver has no shared report source")
	}

	baseCtx, cancel := context.WithCancel(context.Background())
	session := &ReceiverSession{
		receiver:     selected,
		client:       client,
		callbacks:    options.Callbacks,
		childTimeout: options.ChildTimeout,
		baseCtx:      baseCtx,
		cancel:       cancel,
		children:     make(map[byte]*ChildDevice),
		reports:      make(chan hidpp.Report, 128),
		init:         make(chan *ChildDevice, 32),
		done:         make(chan struct{}),
		workerDone:   make(chan struct{}),
	}
	if err := selected.SetReportHandler(session.acceptReport); err != nil {
		cancel()
		_ = selected.Close()
		return nil, err
	}
	go session.worker()

	if err := selected.Configure(ctx); err != nil {
		_ = session.Close()
		return nil, err
	}
	paired, err := selected.EnumeratePaired(ctx)
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	for _, device := range paired {
		child, added := session.addPaired(device)
		if added {
			session.queueInitialization(child)
		}
	}
	return session, nil
}

// NewSession is an explicit lifecycle constructor alias.
func NewSession(ctx context.Context, options LifecycleOptions) (*ReceiverSession, error) {
	return OpenSession(ctx, options)
}

// Receiver returns the selected persistent receiver.
func (s *ReceiverSession) Receiver() *Receiver {
	if s == nil {
		return nil
	}
	return s.receiver
}

type sessionHealth interface {
	Done() <-chan struct{}
	Err() error
}

// Done is closed when the physical receiver session stops. A lifecycle owner
// can use it to distinguish a receiver loss from an ordinary child event and
// begin discovery again. It also closes when Close is called.
func (s *ReceiverSession) Done() <-chan struct{} {
	if s == nil {
		return nil
	}
	if health, ok := s.client.(sessionHealth); ok {
		return health.Done()
	}
	return s.done
}

// Err returns the underlying transport error when the receiver session has
// become unusable. It is nil while the session is active.
func (s *ReceiverSession) Err() error {
	if s == nil {
		return nil
	}
	if health, ok := s.client.(sessionHealth); ok {
		return health.Err()
	}
	s.stateMu.RLock()
	closed := s.closed
	s.stateMu.RUnlock()
	if closed {
		return errors.New("receiver: session closed")
	}
	return nil
}

// Child returns the current child for a wireless index.
func (s *ReceiverSession) Child(index byte) (*ChildDevice, bool) {
	if s == nil {
		return nil, false
	}
	s.stateMu.RLock()
	child, ok := s.children[index]
	s.stateMu.RUnlock()
	return child, ok
}

// Children returns a stable index-ordered snapshot of known children,
// including sleeping children and excluding removed children.
func (s *ReceiverSession) Children() []*ChildDevice {
	if s == nil {
		return nil
	}
	s.stateMu.RLock()
	result := make([]*ChildDevice, 0, len(s.children))
	for _, child := range s.children {
		result = append(result, child)
	}
	s.stateMu.RUnlock()
	sort.Slice(result, func(i, j int) bool {
		return result[i].Metadata().WirelessIndex < result[j].Metadata().WirelessIndex
	})
	return result
}

// Close stops report routing, unblocks any child or receiver transactions via
// the shared transport close, and releases every child handle. It is safe to
// call concurrently and more than once.
func (s *ReceiverSession) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.cancel()
		s.stateMu.Lock()
		s.closed = true
		s.stateMu.Unlock()
		_ = s.receiver.SetReportHandler(nil)
		close(s.done)
		s.closeError = s.receiver.Close()
		<-s.workerDone
		s.stateMu.Lock()
		children := make([]*ChildDevice, 0, len(s.children))
		for _, child := range s.children {
			children = append(children, child)
		}
		s.children = make(map[byte]*ChildDevice)
		s.stateMu.Unlock()
		for _, child := range children {
			child.stateMu.Lock()
			child.state = ChildStateRemoved
			child.stateMu.Unlock()
			_ = child.client.Close()
		}
	})
	return s.closeError
}

func (s *ReceiverSession) acceptReport(report hidpp.Report) {
	select {
	case s.reports <- report:
	case <-s.done:
	}
}

func (s *ReceiverSession) worker() {
	defer close(s.workerDone)
	for {
		select {
		case report := <-s.reports:
			s.handleReport(report)
		case child := <-s.init:
			s.initialize(child)
		case <-s.done:
			return
		}
	}
}

func (s *ReceiverSession) handleReport(report hidpp.Report) {
	if report.SubID == connectionNotification || report.SubID == unpairNotification {
		event, err := ParseDeviceEvent(report)
		if err == nil {
			s.handleDeviceEvent(event)
		}
		return
	}
	s.stateMu.RLock()
	child := s.children[report.DeviceIndex]
	s.stateMu.RUnlock()
	if child != nil {
		child.client.DispatchReport(report)
	}
}

func (s *ReceiverSession) handleDeviceEvent(event DeviceEvent) {
	if !event.Paired {
		s.removeChild(event.Slot)
		return
	}
	metadata := ChildMetadata{
		WirelessIndex: event.Slot,
		PID:           event.PID,
		DeviceType:    event.DeviceType,
		ReceiverKind:  s.receiver.Kind(),
		ReceiverPath:  s.receiver.Metadata().Path,
	}
	child, added := s.upsert(metadata)
	if added {
		s.emit(ChildAdded, child)
	}
	if event.Sleeping || !event.Connected {
		s.setSleeping(child)
		return
	}
	wasSleeping := child.State() == ChildStateSleeping
	if wasSleeping {
		s.emit(ChildWoken, child)
	}
	s.queueInitialization(child)
}

func (s *ReceiverSession) addPaired(device PairedDevice) (*ChildDevice, bool) {
	metadata := ChildMetadata{
		WirelessIndex: device.Slot,
		PID:           device.PID,
		DeviceType:    device.DeviceType,
		Name:          device.Name,
		ReceiverKind:  s.receiver.Kind(),
		ReceiverPath:  s.receiver.Metadata().Path,
	}
	child, added := s.upsert(metadata)
	if added {
		s.emit(ChildAdded, child)
	}
	return child, added
}

func (s *ReceiverSession) upsert(metadata ChildMetadata) (*ChildDevice, bool) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if child := s.children[metadata.WirelessIndex]; child != nil {
		child.stateMu.Lock()
		if metadata.PID != 0 {
			child.meta.PID = metadata.PID
		}
		if metadata.DeviceType != DeviceTypeUnknown {
			child.meta.DeviceType = metadata.DeviceType
		}
		if metadata.Name != "" {
			child.meta.Name = metadata.Name
		}
		child.stateMu.Unlock()
		return child, false
	}
	client, err := hidpp.NewDeviceSession(s.client, nil, metadata.WirelessIndex)
	if err != nil {
		return nil, false
	}
	child := &ChildDevice{state: ChildStateAdded, meta: metadata, client: client}
	s.children[metadata.WirelessIndex] = child
	return child, true
}

func (s *ReceiverSession) queueInitialization(child *ChildDevice) {
	if child == nil {
		return
	}
	select {
	case s.init <- child:
	case <-s.done:
	}
}

func (s *ReceiverSession) initialize(child *ChildDevice) {
	child.stateMu.Lock()
	if child.state == ChildStateRemoved || child.initing {
		child.stateMu.Unlock()
		return
	}
	child.initing = true
	child.stateMu.Unlock()
	defer func() {
		child.stateMu.Lock()
		child.initing = false
		child.stateMu.Unlock()
	}()

	child.client.Reset()
	ctx := s.baseCtx
	var cancel context.CancelFunc
	if s.childTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, s.childTimeout)
		defer cancel()
	}
	version, err := child.client.Validate(ctx)
	if err != nil {
		return
	}
	child.stateMu.Lock()
	if child.state != ChildStateRemoved {
		child.meta.Protocol = version
		child.state = ChildStateReady
	}
	removed := child.state == ChildStateRemoved
	child.stateMu.Unlock()
	if !removed {
		s.emit(ChildReady, child)
	}
}

func (s *ReceiverSession) setSleeping(child *ChildDevice) {
	child.stateMu.Lock()
	if child.state == ChildStateRemoved || child.state == ChildStateSleeping {
		child.stateMu.Unlock()
		return
	}
	child.state = ChildStateSleeping
	child.stateMu.Unlock()
	s.emit(ChildSleeping, child)
}

func (s *ReceiverSession) removeChild(index byte) {
	s.stateMu.Lock()
	child := s.children[index]
	if child != nil {
		delete(s.children, index)
	}
	s.stateMu.Unlock()
	if child == nil {
		return
	}
	child.stateMu.Lock()
	child.state = ChildStateRemoved
	child.stateMu.Unlock()
	_ = child.client.Close()
	s.emit(ChildRemoved, child)
}

func (s *ReceiverSession) emit(eventType ChildEventType, child *ChildDevice) {
	if child == nil {
		return
	}
	event := ChildEvent{Type: eventType, Child: child, Metadata: child.Metadata()}
	if s.callbacks.Event != nil {
		s.callbacks.Event(event)
	}
	switch eventType {
	case ChildAdded:
		if s.callbacks.OnChildAdded != nil {
			s.callbacks.OnChildAdded(child)
		}
	case ChildReady:
		if s.callbacks.OnChildReady != nil {
			s.callbacks.OnChildReady(child)
		}
	case ChildSleeping:
		if s.callbacks.OnChildSleeping != nil {
			s.callbacks.OnChildSleeping(child)
		}
	case ChildWoken:
		if s.callbacks.OnChildWoken != nil {
			s.callbacks.OnChildWoken(child)
		}
	case ChildRemoved:
		if s.callbacks.OnChildRemoved != nil {
			s.callbacks.OnChildRemoved(child)
		}
	}
}
