package hidpp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

const (
	// RootFeatureID is the HID++ 2.0 root feature identifier.
	RootFeatureID uint16 = 0x0000
	// RootFeatureIndex is fixed by the HID++ 2.0 protocol.
	RootFeatureIndex byte = 0x00
	// FeatureErrorSubID is the HID++ 2.0 error feature index.
	FeatureErrorSubID byte = 0xff
	// RootProtocolSubID is the short-report root protocol request sub-ID.
	RootProtocolSubID byte = 0x00
	// RootProtocolCommand is the root protocol-version command byte.
	RootProtocolCommand byte = 0x10
	// RootPingByte is echoed by a HID++ 2.0 root protocol-version response.
	RootPingByte byte = 0x5a
	// ClientSoftwareID identifies normal requests made by this implementation.
	// HID++ reserves zero for firmware/internal use on devices that require a
	// client software ID.
	ClientSoftwareID byte = 0x02
	// NoResponseSoftwareID identifies commands deliberately sent without an
	// acknowledgement waiter.
	NoResponseSoftwareID byte = 0x03
)

// ExchangeClient is the portion of Session needed by a child device client.
// Keeping this interface small permits hardware-free protocol tests.
type ExchangeClient interface {
	Exchange(context.Context, Request) (Report, error)
	Send(context.Context, Report) error
}

// ReportSubscriber receives reports that were not consumed by a transaction.
// Session implements this interface. A nil subscriber is allowed when a
// caller routes reports itself, as ReceiverSession does.
type ReportSubscriber interface {
	SubscribeReport(func(Report)) func()
}

// ProtocolVersion is the version returned by the HID++ root protocol ping.
type ProtocolVersion struct {
	Major byte
	Minor byte
}

// FeatureInfo describes one dynamically discovered feature.
type FeatureInfo struct {
	ID      uint16
	Index   byte
	Type    byte
	Version byte
}

// DeviceSession is a HID++ 2.0 child-device client over a shared Session.
// It never owns or closes the physical transport. Its index is the sole
// routing key that distinguishes it from the receiver and sibling children.
type DeviceSession struct {
	client      ExchangeClient
	deviceIndex byte

	stateMu    sync.RWMutex
	closed     bool
	version    ProtocolVersion
	hasVersion bool

	featureMu sync.Mutex
	features  map[uint16]FeatureInfo

	handlerMu   sync.RWMutex
	nextHandler uint64
	handlers    map[uint64]func(Report)
	unsub       func()
}

// NewDeviceSession creates a child client for deviceIndex. The receiver index
// 0xff is reserved for the physical receiver and is rejected here.
func NewDeviceSession(client ExchangeClient, reports ReportSubscriber, deviceIndex byte) (*DeviceSession, error) {
	if client == nil {
		return nil, errors.New("hidpp: nil child exchange client")
	}
	if deviceIndex == 0 || deviceIndex == 0xff {
		return nil, &UnsupportedError{
			Operation: "HID++ 2.0 child device index",
			Detail:    fmt.Sprintf("0x%02x is not a wireless child index", deviceIndex),
		}
	}
	d := &DeviceSession{
		client:      client,
		deviceIndex: deviceIndex,
		features:    make(map[uint16]FeatureInfo),
		handlers:    make(map[uint64]func(Report)),
	}
	d.features[RootFeatureID] = FeatureInfo{ID: RootFeatureID, Index: RootFeatureIndex}
	if reports != nil {
		d.unsub = reports.SubscribeReport(func(report Report) { d.DispatchReport(report) })
	}
	return d, nil
}

// DeviceIndex returns the wireless index used by this child.
func (d *DeviceSession) DeviceIndex() byte {
	if d == nil {
		return 0
	}
	return d.deviceIndex
}

// Version returns the last successfully validated protocol version.
func (d *DeviceSession) Version() (ProtocolVersion, bool) {
	if d == nil {
		return ProtocolVersion{}, false
	}
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.version, d.hasVersion
}

// Ping performs the HID++ root protocol-version exchange. The device must
// echo the protocol ping byte; the returned major/minor values are otherwise
// left to Validate to interpret.
func (d *DeviceSession) Ping(ctx context.Context) (ProtocolVersion, error) {
	if err := d.checkUsable(); err != nil {
		return ProtocolVersion{}, err
	}
	command := (RootProtocolCommand & 0xf0) | ClientSoftwareID
	response, err := d.client.Exchange(ctx, Request{Report: Report{
		Type:        ReportTypeShort,
		DeviceIndex: d.deviceIndex,
		SubID:       RootProtocolSubID,
		Function:    command >> 4,
		SoftwareID:  command & 0x0f,
		Parameters:  []byte{0x00, 0x00, RootPingByte},
	}, ResponseSubID: RootProtocolSubID})
	if err != nil {
		return ProtocolVersion{}, err
	}
	if len(response.Parameters) < 3 {
		return ProtocolVersion{}, malformedResponse(nil, fmt.Errorf("root protocol response has %d parameters, need 3", len(response.Parameters)))
	}
	if response.Parameters[2] != RootPingByte {
		return ProtocolVersion{}, &ProtocolError{
			DeviceIndex:    d.deviceIndex,
			Code:           response.Parameters[2],
			RequestSubID:   RootProtocolSubID,
			RequestAddress: command,
			Parameters:     append([]byte(nil), response.Parameters...),
		}
	}
	version := ProtocolVersion{Major: response.Parameters[0], Minor: response.Parameters[1]}
	d.stateMu.Lock()
	d.version = version
	d.hasVersion = true
	d.stateMu.Unlock()
	return version, nil
}

// Validate performs the root ping and rejects devices that do not advertise
// HID++ 2.x. A temporary link failure is returned to the caller unchanged so
// a receiver manager can retain the known child and retry after wake.
func (d *DeviceSession) Validate(ctx context.Context) (ProtocolVersion, error) {
	version, err := d.Ping(ctx)
	if err != nil {
		var protocolErr *ProtocolError
		if errors.As(err, &protocolErr) && protocolErr.Code == 0x01 {
			return ProtocolVersion{}, &UnsupportedError{
				Operation: "HID++ 2.0 child protocol",
				Detail:    "device rejected the root protocol-version command",
			}
		}
		return ProtocolVersion{}, err
	}
	if version.Major < 2 {
		return ProtocolVersion{}, &UnsupportedError{
			Operation: "HID++ 2.0 child protocol",
			Detail:    fmt.Sprintf("device reported HID++ %d.%d", version.Major, version.Minor),
		}
	}
	return version, nil
}

// LookupFeature resolves a 16-bit HID++ feature ID through the root feature.
// Results are cached for the life of the child session and are invalidated by
// Reset, which the receiver lifecycle uses after a wake transition.
func (d *DeviceSession) LookupFeature(ctx context.Context, featureID uint16) (FeatureInfo, error) {
	if err := d.checkUsable(); err != nil {
		return FeatureInfo{}, err
	}
	d.featureMu.Lock()
	defer d.featureMu.Unlock()
	if info, ok := d.features[featureID]; ok {
		return info, nil
	}
	response, err := d.callIndexLocked(ctx, RootFeatureIndex, 0x00, []byte{byte(featureID >> 8), byte(featureID)})
	if err != nil {
		return FeatureInfo{}, err
	}
	if len(response.Parameters) < 1 {
		return FeatureInfo{}, malformedResponse(nil, fmt.Errorf("feature lookup response has %d parameters, need 1", len(response.Parameters)))
	}
	index := response.Parameters[0]
	if index == 0 {
		return FeatureInfo{}, &UnsupportedError{
			Operation: "HID++ feature",
			Detail:    fmt.Sprintf("feature 0x%04x is not present", featureID),
		}
	}
	info := FeatureInfo{ID: featureID, Index: index}
	if len(response.Parameters) > 1 {
		info.Type = response.Parameters[1]
	}
	if len(response.Parameters) > 2 {
		info.Version = response.Parameters[2]
	}
	d.features[featureID] = info
	return info, nil
}

// FeatureIndex is a convenience wrapper around LookupFeature.
func (d *DeviceSession) FeatureIndex(ctx context.Context, featureID uint16) (byte, error) {
	info, err := d.LookupFeature(ctx, featureID)
	if err != nil {
		return 0, err
	}
	return info.Index, nil
}

// Features returns a copy of the currently cached feature information. It
// does not perform I/O or claim that an undiscovered feature is unsupported.
func (d *DeviceSession) Features() []FeatureInfo {
	if d == nil {
		return nil
	}
	d.featureMu.Lock()
	defer d.featureMu.Unlock()
	result := make([]FeatureInfo, 0, len(d.features))
	for _, info := range d.features {
		result = append(result, info)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// Call invokes a feature function by its dynamic feature index. function is
// the complete HID++ command byte: its high nibble is the function and its
// low nibble is the software/client ID. This mirrors the wire representation
// and permits callers to match a particular software ID.
func (d *DeviceSession) Call(ctx context.Context, featureIndex, function byte, params []byte) ([]byte, error) {
	response, err := d.callIndex(ctx, featureIndex, function, params)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), response.Parameters...), nil
}

// CallFeature resolves featureID and invokes function on the resulting
// feature index.
func (d *DeviceSession) CallFeature(ctx context.Context, featureID uint16, function byte, params []byte) ([]byte, error) {
	info, err := d.LookupFeature(ctx, featureID)
	if err != nil {
		return nil, err
	}
	return d.Call(ctx, info.Index, function, params)
}

// CallWithSoftwareID is the explicit-nibble form of Call.
func (d *DeviceSession) CallWithSoftwareID(ctx context.Context, featureIndex, function, softwareID byte, params []byte) ([]byte, error) {
	if function > 0x0f || softwareID == 0 || softwareID > 0x0f {
		return nil, unsupportedFeatureCommand(function, softwareID)
	}
	return d.Call(ctx, featureIndex, function<<4|softwareID, params)
}

// CallNoResponse sends a feature command without registering a response
// waiter. It is safe on the shared stream because Send still uses Session's
// write gate; any unsolicited report remains available to subscriptions.
func (d *DeviceSession) CallNoResponse(ctx context.Context, featureIndex, function byte, params []byte) error {
	if err := d.checkUsable(); err != nil {
		return err
	}
	if len(params) > longParameterLen {
		return unsupportedFeaturePayload(len(params))
	}
	return d.client.Send(ctx, Report{
		Type:        ReportTypeLong,
		DeviceIndex: d.deviceIndex,
		SubID:       featureIndex,
		Function:    function >> 4,
		SoftwareID:  NoResponseSoftwareID,
		Parameters:  append([]byte(nil), params...),
	})
}

// CallFeatureNoResponse resolves featureID and sends a no-response command.
func (d *DeviceSession) CallFeatureNoResponse(ctx context.Context, featureID uint16, function byte, params []byte) error {
	info, err := d.LookupFeature(ctx, featureID)
	if err != nil {
		return err
	}
	return d.CallNoResponse(ctx, info.Index, function, params)
}

// SubscribeEvents registers a handler for unsolicited HID++ 2.0 reports from
// this child. The callback runs on the shared session reader goroutine.
func (d *DeviceSession) SubscribeEvents(handler func(Report)) func() {
	if d == nil || handler == nil {
		return func() {}
	}
	d.handlerMu.Lock()
	d.nextHandler++
	id := d.nextHandler
	d.handlers[id] = handler
	d.handlerMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			d.handlerMu.Lock()
			delete(d.handlers, id)
			d.handlerMu.Unlock()
		})
	}
}

// SetEventHandler is the one-handler convenience form of SubscribeEvents.
// The returned cancellation function removes the handler.
func (d *DeviceSession) SetEventHandler(handler func(Report)) func() {
	return d.SubscribeEvents(handler)
}

// DispatchReport routes a report supplied by an owner of the shared reader.
// It returns true when the report belongs to this child and is an unsolicited
// feature report. It intentionally ignores responses and protocol errors.
func (d *DeviceSession) DispatchReport(report Report) bool {
	if d == nil || report.DeviceIndex != d.deviceIndex || report.Type != ReportTypeLong || report.SubID == FeatureErrorSubID {
		return false
	}
	d.stateMu.RLock()
	closed := d.closed
	d.stateMu.RUnlock()
	if closed {
		return false
	}
	d.handlerMu.RLock()
	handlers := make([]func(Report), 0, len(d.handlers))
	for _, handler := range d.handlers {
		handlers = append(handlers, handler)
	}
	d.handlerMu.RUnlock()
	for _, handler := range handlers {
		handler(report)
	}
	return true
}

// Reset clears protocol validation and dynamic feature state without closing
// the child. It is used when a known device wakes or reconnects.
func (d *DeviceSession) Reset() {
	if d == nil {
		return
	}
	d.stateMu.Lock()
	d.version = ProtocolVersion{}
	d.hasVersion = false
	d.stateMu.Unlock()
	d.featureMu.Lock()
	d.features = map[uint16]FeatureInfo{RootFeatureID: {ID: RootFeatureID, Index: RootFeatureIndex}}
	d.featureMu.Unlock()
}

// Close releases event subscriptions owned by the child. It deliberately does
// not close the shared physical transport.
func (d *DeviceSession) Close() error {
	if d == nil {
		return nil
	}
	d.stateMu.Lock()
	if d.closed {
		d.stateMu.Unlock()
		return nil
	}
	d.closed = true
	d.stateMu.Unlock()
	if d.unsub != nil {
		d.unsub()
	}
	d.handlerMu.Lock()
	d.handlers = make(map[uint64]func(Report))
	d.handlerMu.Unlock()
	return nil
}

func (d *DeviceSession) callIndex(ctx context.Context, featureIndex, function byte, params []byte) (Report, error) {
	if err := d.checkUsable(); err != nil {
		return Report{}, err
	}
	if len(params) > longParameterLen {
		return Report{}, unsupportedFeaturePayload(len(params))
	}
	return d.callIndexLocked(ctx, featureIndex, function, params)
}

func (d *DeviceSession) callIndexLocked(ctx context.Context, featureIndex, function byte, params []byte) (Report, error) {
	function = normalFeatureCommand(function)
	request := Report{
		Type:        ReportTypeLong,
		DeviceIndex: d.deviceIndex,
		SubID:       featureIndex,
		Function:    function >> 4,
		SoftwareID:  function & 0x0f,
		Parameters:  append([]byte(nil), params...),
	}
	return d.client.Exchange(ctx, Request{Report: request, ResponseSubID: featureIndex})
}

func normalFeatureCommand(command byte) byte {
	if command&0x0f == 0 {
		return command | ClientSoftwareID
	}
	return command
}

func (d *DeviceSession) checkUsable() error {
	if d == nil {
		return errors.New("hidpp: nil child session")
	}
	d.stateMu.RLock()
	closed := d.closed
	d.stateMu.RUnlock()
	if closed {
		return &ClosedTransportError{Cause: errors.New("child session closed")}
	}
	return nil
}

func unsupportedFeaturePayload(size int) error {
	return &UnsupportedError{
		Operation: "HID++ 2.0 feature payload",
		Detail:    fmt.Sprintf("%d bytes exceeds the long report capacity of %d", size, longParameterLen),
	}
}

func unsupportedFeatureCommand(function, softwareID byte) error {
	return &UnsupportedError{
		Operation: "HID++ 2.0 function/software ID",
		Detail:    fmt.Sprintf("function 0x%02x and software ID 0x%02x must fit in four bits", function, softwareID),
	}
}
