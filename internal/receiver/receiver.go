// Package receiver implements the HID++ 1.0 receiver side of Bolt and
// Unifying protocols and owns persistent shared-stream child lifecycles.
package receiver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"unicode/utf8"

	"github.com/atremb/logitechd/internal/hidpp"
	"github.com/atremb/logitechd/internal/hidraw"
)

const (
	// ReceiverDeviceIndex is the HID++ address used for commands sent to the
	// receiver itself rather than to one of its wireless children.
	ReceiverDeviceIndex byte = 0xff

	// Receiver registers used by both receiver families.
	NotificationRegister uint16 = 0x00
	ConnectionRegister   uint16 = 0x02
	ReceiverInfoRegister uint16 = 0x2b5

	// BoltUniqueIDRegister is present on Bolt receivers and is useful for
	// distinguishing them from older receiver firmware.
	BoltUniqueIDRegister uint16 = 0x2fb

	// Known product IDs only affect which probe is attempted first. Unknown
	// product IDs still receive both protocol probes.
	BoltReceiverProductID      uint16 = 0xc548
	UnifyingReceiverProductID1 uint16 = 0xc52b
	UnifyingReceiverProductID2 uint16 = 0xc532
)

const (
	connectionNotification byte = 0x41
	unpairNotification     byte = 0x40
	boltPairInfoBase       byte = 0x50
	boltNameBase           byte = 0x60
	unifyingPairInfoBase   byte = 0x20
	unifyingNameBase       byte = 0x40
	maxReceiverSlots       byte = 6
	maxDeviceNameBytes          = 14
)

var (
	// ErrNotReceiver means that an opened HIDRAW node did not answer the
	// receiver-specific HID++ 1.0 probes.
	ErrNotReceiver = errors.New("receiver: not a supported receiver")
	// ErrNoReceiver means that discovery did not find a usable receiver.
	ErrNoReceiver = errors.New("receiver: no receiver found")
	// ErrInvalidSlot is returned instead of allowing an out-of-range slot to
	// become a protocol request.
	ErrInvalidSlot = errors.New("receiver: invalid device slot")
)

// Kind identifies the two receiver register dialects supported here.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindBolt
	KindUnifying
)

func (k Kind) String() string {
	switch k {
	case KindBolt:
		return "bolt"
	case KindUnifying:
		return "unifying"
	default:
		return "unknown"
	}
}

// DeviceType is the HID++ 1.0 device-kind nibble from pairing information.
// Unknown values are retained so newer firmware does not make enumeration
// fail merely because this package has not named a new kind yet.
type DeviceType byte

const (
	DeviceTypeUnknown   DeviceType = 0x00
	DeviceTypeKeyboard  DeviceType = 0x01
	DeviceTypeMouse     DeviceType = 0x02
	DeviceTypeNumpad    DeviceType = 0x03
	DeviceTypePresenter DeviceType = 0x04
	DeviceTypeRemote    DeviceType = 0x07
	DeviceTypeTrackball DeviceType = 0x08
	DeviceTypeTouchpad  DeviceType = 0x09
	DeviceTypeTablet    DeviceType = 0x0a
	DeviceTypeGamepad   DeviceType = 0x0b
	DeviceTypeJoystick  DeviceType = 0x0c
	DeviceTypeHeadset   DeviceType = 0x0d
)

func (d DeviceType) String() string {
	names := map[DeviceType]string{
		DeviceTypeUnknown:   "unknown",
		DeviceTypeKeyboard:  "keyboard",
		DeviceTypeMouse:     "mouse",
		DeviceTypeNumpad:    "numpad",
		DeviceTypePresenter: "presenter",
		DeviceTypeRemote:    "remote",
		DeviceTypeTrackball: "trackball",
		DeviceTypeTouchpad:  "touchpad",
		DeviceTypeTablet:    "tablet",
		DeviceTypeGamepad:   "gamepad",
		DeviceTypeJoystick:  "joystick",
		DeviceTypeHeadset:   "headset",
	}
	if name, ok := names[d]; ok {
		return name
	}
	return fmt.Sprintf("unknown(0x%02x)", byte(d))
}

// DeviceMetadata is optional USB information associated with a HIDRAW path.
// Protocol probing remains authoritative; metadata only changes probe order.
type DeviceMetadata struct {
	Path      string
	BusType   uint32
	VendorID  uint16
	ProductID uint16
}

// PairedDevice is the subset of pairing information needed by this phase.
// PID is the receiver's two-byte wireless product identifier, not a USB PID.
type PairedDevice struct {
	Slot       byte
	PID        uint16
	DeviceType DeviceType
	Name       string
}

// FindPairedDevice selects a paired device by exact name and, when non-zero,
// slot. A zero slot is a wildcard. The slot check makes a duplicate or
// truncated display name harmless when a caller already knows the receiver
// index.
func FindPairedDevice(devices []PairedDevice, name string, slot byte) (PairedDevice, bool) {
	for _, device := range devices {
		if device.Name == name && (slot == 0 || device.Slot == slot) {
			return device, true
		}
	}
	return PairedDevice{}, false
}

// DeviceEvent is a connection, disconnection, or unpair notification emitted
// by a receiver. Slot is the HID++ device index. HasPID is false for the
// short unpair form, which carries no product identifier.
type DeviceEvent struct {
	Slot      byte
	Connected bool
	// Sleeping means the receiver still considers the slot paired but the
	// wireless link is temporarily unavailable. It is distinct from removal.
	Sleeping        bool
	Paired          bool
	HasPID          bool
	PID             uint16
	DeviceType      DeviceType
	Encrypted       bool
	ProtocolAddress byte
}

// ParseDeviceEvent decodes the HID++ 1.0 receiver notifications used by Bolt
// and Unifying receivers. The report's DeviceIndex is the wireless slot.
func ParseDeviceEvent(report hidpp.Report) (DeviceEvent, error) {
	if err := validateSlot(report.DeviceIndex); err != nil {
		return DeviceEvent{}, err
	}
	event := DeviceEvent{
		Slot:            report.DeviceIndex,
		ProtocolAddress: report.CommandByte(),
		DeviceType:      DeviceTypeUnknown,
	}

	switch report.SubID {
	case connectionNotification:
		if len(report.Parameters) < 3 {
			return DeviceEvent{}, malformed("connection notification", len(report.Parameters), 3)
		}
		flags := report.Parameters[0]
		event.Connected = flags&0x40 == 0
		event.Sleeping = !event.Connected
		event.Paired = true
		event.HasPID = true
		event.PID = uint16(report.Parameters[2])<<8 | uint16(report.Parameters[1])
		event.DeviceType = DeviceType(flags & 0x0f)
		event.Encrypted = flags&0x20 != 0 || report.CommandByte() == 0x10
		return event, nil
	case unpairNotification:
		// The documented unpair indication uses address 0x02. Treat other
		// addresses as a disconnection too, since older receiver firmware
		// emits a similarly shaped status without the unpair marker.
		event.Connected = false
		event.Paired = report.CommandByte() != 0x02
		event.Sleeping = event.Paired
		if len(report.Parameters) >= 3 && !allZero(report.Parameters) {
			event.HasPID = true
			event.PID = uint16(report.Parameters[2])<<8 | uint16(report.Parameters[1])
			event.DeviceType = DeviceType(report.Parameters[0] & 0x0f)
			event.Encrypted = report.Parameters[0]&0x20 != 0 || report.CommandByte() == 0x10
		} else if len(report.Parameters) != 0 && !allZero(report.Parameters) {
			return DeviceEvent{}, malformed("unpair notification", len(report.Parameters), 0)
		}
		return event, nil
	default:
		return DeviceEvent{}, fmt.Errorf("receiver: unsupported notification sub-ID 0x%02x", report.SubID)
	}
}

// RegisterClient is the Phase 2 HID++ 1.0 register surface required by a
// receiver.
type RegisterClient interface {
	GetRegister(context.Context, byte, uint16, int) ([]byte, error)
	SetRegister(context.Context, byte, uint16, []byte) error
}

// RegisterSelector is the optional extension used by receiver long registers
// that select a subregister in the request payload. hidpp.Session implements
// this alongside the unchanged RegisterClient methods.
type RegisterSelector interface {
	GetRegisterWithParameters(context.Context, byte, uint16, int, ...byte) ([]byte, error)
}

// ReportSource is the single-reader notification hook supplied by the Phase 2
// session. It is intentionally tiny so tests and future transports can inject
// reports without opening a HIDRAW node.
type ReportSource interface {
	SetReportHandler(func(hidpp.Report))
}

// EventHandler receives parsed receiver notifications. It is invoked by the
// report source's reader goroutine, so it should not perform blocking I/O.
type EventHandler func(DeviceEvent, error)

// Opened contains the injectable pieces needed to construct a Receiver.
// Close owns the underlying transport; it is called at most once by Receiver.
type Opened struct {
	Client   RegisterClient
	Reports  ReportSource
	Metadata DeviceMetadata
	Close    func() error
}

// Receiver is one opened and probed receiver. Its methods are safe for a
// normal one-owner lifecycle; Close itself is safe to call concurrently.
type Receiver struct {
	client   RegisterClient
	reports  ReportSource
	metadata DeviceMetadata
	closer   func() error

	stateMu sync.Mutex
	kind    Kind
	closed  bool

	closeOnce  sync.Once
	closeError error
}

// New constructs a receiver around injected register and report clients.
func New(opened Opened) (*Receiver, error) {
	if opened.Client == nil {
		return nil, errors.New("receiver: nil register client")
	}
	if opened.Close == nil {
		opened.Close = func() error { return nil }
	}
	return &Receiver{
		client:   opened.Client,
		reports:  opened.Reports,
		metadata: opened.Metadata,
		closer:   opened.Close,
	}, nil
}

// Metadata returns a copy of the optional path and USB identification.
func (r *Receiver) Metadata() DeviceMetadata {
	if r == nil {
		return DeviceMetadata{}
	}
	return r.metadata
}

// Kind returns the result of the most recent successful Probe.
func (r *Receiver) Kind() Kind {
	if r == nil {
		return KindUnknown
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.kind
}

// Probe identifies the receiver using HID++ behavior. The USB product ID is
// only a probe-order hint; an unlisted Bolt or Unifying product is still tried.
func (r *Receiver) Probe(ctx context.Context) (Kind, error) {
	if err := r.checkUsable(); err != nil {
		return KindUnknown, err
	}
	kind, err := probe(ctx, r.client, r.metadata)
	if err != nil {
		return KindUnknown, err
	}
	r.stateMu.Lock()
	r.kind = kind
	r.stateMu.Unlock()
	return kind, nil
}

// SetEventHandler connects receiver notification parsing to the shared report
// source. Passing nil removes the handler. ParseDeviceEvent remains available
// for callers that prefer to dispatch reports themselves.
func (r *Receiver) SetEventHandler(handler EventHandler) error {
	if r == nil {
		return errors.New("receiver: nil receiver")
	}
	if r.reports == nil {
		return errors.New("receiver: no report source")
	}
	if err := r.checkUsable(); err != nil {
		return err
	}
	if handler == nil {
		r.reports.SetReportHandler(nil)
		return nil
	}
	return r.SetReportHandler(func(report hidpp.Report) {
		if report.SubID != connectionNotification && report.SubID != unpairNotification {
			return
		}
		event, err := ParseDeviceEvent(report)
		handler(event, err)
	})
}

// SetReportHandler installs a handler for every report not consumed by a
// transaction on the shared report source. ReceiverSession uses this to
// route both receiver notifications and unsolicited child HID++ 2.0 reports
// without creating a second reader.
func (r *Receiver) SetReportHandler(handler func(hidpp.Report)) error {
	if r == nil {
		return errors.New("receiver: nil receiver")
	}
	if r.reports == nil {
		return errors.New("receiver: no report source")
	}
	if err := r.checkUsable(); err != nil {
		return err
	}
	r.reports.SetReportHandler(handler)
	return nil
}

// EnableNotifications requests wireless connection status notifications from
// the receiver. The three flag bytes are the HID++ 1.0 big-endian 24-bit
// notification register; bits 8 and 11 are the wireless-status and
// software-present receiver flags respectively.
func (r *Receiver) EnableNotifications(ctx context.Context) error {
	if err := r.checkUsable(); err != nil {
		return err
	}
	return r.client.SetRegister(ctx, ReceiverDeviceIndex, NotificationRegister, []byte{0x00, 0x09, 0x00})
}

// RequestStartupEnumeration asks the receiver to emit status for its paired
// slots. It does not start discovery or pairing.
func (r *Receiver) RequestStartupEnumeration(ctx context.Context) error {
	if err := r.checkUsable(); err != nil {
		return err
	}
	return r.client.SetRegister(ctx, ReceiverDeviceIndex, ConnectionRegister, []byte{0x02})
}

// Configure performs the two receiver-side setup writes needed before a
// daemon begins consuming connection notifications.
func (r *Receiver) Configure(ctx context.Context) error {
	if r.Kind() == KindUnknown {
		if _, err := r.Probe(ctx); err != nil {
			return err
		}
	}
	if err := r.EnableNotifications(ctx); err != nil {
		return fmt.Errorf("receiver: enable notifications: %w", err)
	}
	if err := r.RequestStartupEnumeration(ctx); err != nil {
		return fmt.Errorf("receiver: request startup enumeration: %w", err)
	}
	return nil
}

// EnumeratePaired reads slots one through six. Empty slots are skipped when a
// receiver returns the standard unknown-device protocol error. Name reads are
// optional because some older firmware exposes pairing data but no name.
func (r *Receiver) EnumeratePaired(ctx context.Context) ([]PairedDevice, error) {
	if err := r.checkUsable(); err != nil {
		return nil, err
	}
	kind := r.Kind()
	if kind == KindUnknown {
		var err error
		kind, err = r.Probe(ctx)
		if err != nil {
			return nil, err
		}
	}

	devices := make([]PairedDevice, 0, maxReceiverSlots)
	for slot := byte(1); slot <= maxReceiverSlots; slot++ {
		device, present, err := r.readPairedDevice(ctx, kind, slot)
		if err != nil {
			if isEmptySlotError(err) {
				continue
			}
			return nil, fmt.Errorf("receiver: inspect slot %d: %w", slot, err)
		}
		if present {
			devices = append(devices, device)
		}
	}
	return devices, nil
}

// Close releases the opened transport. It is safe and idempotent.
func (r *Receiver) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		r.stateMu.Lock()
		r.closed = true
		r.stateMu.Unlock()
		if r.reports != nil {
			r.reports.SetReportHandler(nil)
		}
		r.closeError = r.closer()
	})
	return r.closeError
}

func (r *Receiver) readPairedDevice(ctx context.Context, kind Kind, slot byte) (PairedDevice, bool, error) {
	if err := validateSlot(slot); err != nil {
		return PairedDevice{}, false, err
	}
	var (
		data []byte
		err  error
	)
	switch kind {
	case KindBolt:
		data, err = r.getRegister(ctx, ReceiverInfoRegister, 8, boltPairInfoBase+slot)
	case KindUnifying:
		data, err = r.getRegister(ctx, ReceiverInfoRegister, 8, unifyingPairInfoBase+slot-1)
	default:
		return PairedDevice{}, false, ErrNotReceiver
	}
	if err != nil {
		return PairedDevice{}, false, err
	}
	device, present, err := decodePairingInfo(kind, slot, data)
	if err != nil || !present {
		return device, present, err
	}

	name, err := r.readName(ctx, kind, slot)
	if err != nil {
		if !isOptionalNameError(err) {
			return PairedDevice{}, false, err
		}
	} else {
		device.Name = name
	}
	return device, true, nil
}

func (r *Receiver) getRegister(ctx context.Context, address uint16, size int, params ...byte) ([]byte, error) {
	return getRegister(ctx, r.client, address, size, params...)
}

func getRegister(ctx context.Context, client RegisterClient, address uint16, size int, params ...byte) ([]byte, error) {
	if len(params) == 0 {
		return client.GetRegister(ctx, ReceiverDeviceIndex, address, size)
	}
	selector, ok := client.(RegisterSelector)
	if !ok {
		return nil, fmt.Errorf("receiver: register client cannot select long register subregister")
	}
	return selector.GetRegisterWithParameters(ctx, ReceiverDeviceIndex, address, size, params...)
}

func (r *Receiver) readName(ctx context.Context, kind Kind, slot byte) (string, error) {
	var (
		data []byte
		err  error
	)
	switch kind {
	case KindBolt:
		data, err = r.getRegister(ctx, ReceiverInfoRegister, 16, boltNameBase+slot, 0x01)
		if err != nil {
			return "", err
		}
		return decodeName(data, 2, 3)
	case KindUnifying:
		data, err = r.getRegister(ctx, ReceiverInfoRegister, 16, unifyingNameBase+slot-1)
		if err != nil {
			return "", err
		}
		return decodeName(data, 1, 2)
	default:
		return "", ErrNotReceiver
	}
}

func decodePairingInfo(kind Kind, slot byte, data []byte) (PairedDevice, bool, error) {
	if len(data) == 0 {
		return PairedDevice{}, false, malformed("pairing information", 0, 1)
	}
	switch kind {
	case KindBolt:
		if len(data) < 4 {
			return PairedDevice{}, false, malformed("Bolt pairing information", len(data), 4)
		}
	case KindUnifying:
		if len(data) < 8 {
			return PairedDevice{}, false, malformed("Unifying pairing information", len(data), 8)
		}
	default:
		return PairedDevice{}, false, ErrNotReceiver
	}
	if allZero(data) {
		return PairedDevice{}, false, nil
	}
	device := PairedDevice{Slot: slot}
	switch kind {
	case KindBolt:
		device.PID = uint16(data[3])<<8 | uint16(data[2])
		device.DeviceType = DeviceType(data[1] & 0x0f)
	case KindUnifying:
		device.PID = uint16(data[3])<<8 | uint16(data[4])
		device.DeviceType = DeviceType(data[7] & 0x0f)
	}
	if device.PID == 0 {
		return PairedDevice{}, false, nil
	}
	return device, true, nil
}

func decodeName(data []byte, lengthOffset, textOffset int) (string, error) {
	if lengthOffset < 0 || textOffset < 0 || lengthOffset >= len(data) || textOffset > len(data) {
		return "", malformed("device name", len(data), textOffset+1)
	}
	length := int(data[lengthOffset])
	if length > maxDeviceNameBytes || length > len(data)-textOffset {
		return "", malformed("device name length", length, len(data)-textOffset)
	}
	name := bytes.TrimRight(data[textOffset:textOffset+length], "\x00")
	if !utf8.Valid(name) {
		return "", fmt.Errorf("%w: receiver: device name is not valid UTF-8", hidpp.ErrMalformedResponse)
	}
	return string(name), nil
}

func decodeProbeInfo(data []byte) error {
	if len(data) < 7 {
		return malformed("receiver information", len(data), 7)
	}
	return nil
}

func probe(ctx context.Context, client RegisterClient, metadata DeviceMetadata) (Kind, error) {
	if client == nil {
		return KindUnknown, errors.New("receiver: nil register client")
	}
	connection, err := client.GetRegister(ctx, ReceiverDeviceIndex, ConnectionRegister, 1)
	if err != nil {
		return KindUnknown, fmt.Errorf("%w: receiver connection probe: %v", ErrNotReceiver, err)
	}
	if len(connection) < 1 {
		return KindUnknown, malformed("receiver connection", len(connection), 1)
	}

	knownUnifying := metadata.ProductID == UnifyingReceiverProductID1 || metadata.ProductID == UnifyingReceiverProductID2
	knownBolt := metadata.ProductID == BoltReceiverProductID
	tryBoltFirst := knownBolt || !knownUnifying
	var first, second Kind
	if tryBoltFirst {
		first, second = KindBolt, KindUnifying
	} else {
		first, second = KindUnifying, KindBolt
	}
	for _, candidate := range []Kind{first, second} {
		switch candidate {
		case KindBolt:
			data, err := client.GetRegister(ctx, ReceiverDeviceIndex, BoltUniqueIDRegister, 1)
			if err == nil {
				if len(data) == 0 {
					return KindUnknown, malformed("Bolt identity", 0, 1)
				}
				return KindBolt, nil
			}
		case KindUnifying:
			data, err := getRegister(ctx, client, ReceiverInfoRegister, 7, 0x03)
			if err == nil {
				if err := decodeProbeInfo(data); err != nil {
					return KindUnknown, err
				}
				return KindUnifying, nil
			}
		}
	}
	return KindUnknown, ErrNotReceiver
}

func isEmptySlotError(err error) bool {
	var protocolErr *hidpp.ProtocolError
	if !errors.As(err, &protocolErr) {
		return false
	}
	// Unknown device is the usual empty-slot response. A few older receiver
	// firmwares use invalid-address/request-unavailable for the same query.
	switch protocolErr.Code {
	case 0x02, 0x08, 0x0a:
		return true
	default:
		return false
	}
}

func isOptionalNameError(err error) bool {
	var protocolErr *hidpp.ProtocolError
	if !errors.As(err, &protocolErr) {
		return false
	}
	return protocolErr.Code == 0x01 || protocolErr.Code == 0x02 || protocolErr.Code == 0x08 || protocolErr.Code == 0x0a
}

func validateSlot(slot byte) error {
	if slot < 1 || slot > maxReceiverSlots {
		return fmt.Errorf("%w: %d (valid range is 1..%d)", ErrInvalidSlot, slot, maxReceiverSlots)
	}
	return nil
}

func malformed(what string, got, want int) error {
	return fmt.Errorf("%w: receiver: malformed %s: got %d bytes, need %d", hidpp.ErrMalformedResponse, what, got, want)
}

func allZero(data []byte) bool {
	return len(data) > 0 && bytes.Equal(data, make([]byte, len(data)))
}

func (r *Receiver) checkUsable() error {
	if r == nil {
		return errors.New("receiver: nil receiver")
	}
	r.stateMu.Lock()
	closed := r.closed
	r.stateMu.Unlock()
	if closed {
		return errors.New("receiver: closed")
	}
	return nil
}

// Scanner finds candidate HIDRAW paths. Implementations may use any source;
// the default scanner only performs a filesystem glob.
type Scanner interface {
	Scan() ([]string, error)
}

// ScannerFunc adapts a function into a Scanner.
type ScannerFunc func() ([]string, error)

func (f ScannerFunc) Scan() ([]string, error) { return f() }

// DeviceScanner is the production /dev/hidraw* scanner.
type DeviceScanner struct {
	Pattern string
}

func (s DeviceScanner) Scan() ([]string, error) {
	pattern := s.Pattern
	if pattern == "" {
		pattern = "/dev/hidraw*"
	}
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("receiver: scan %q: %w", pattern, err)
	}
	sort.Strings(paths)
	return paths, nil
}

// Opener creates the injectable register/report clients for one path.
type Opener func(string) (Opened, error)

// Options controls receiver discovery. Path overrides Scanner when non-empty.
// Opener is normally only supplied by tests or a future platform transport.
type Options struct {
	Path           string
	Scanner        Scanner
	Opener         Opener
	SessionOptions hidpp.SessionOptions
}

// Snapshot is the result of OpenAndEnumerate after its receiver has been
// closed. It contains only copied metadata and paired-device values.
type Snapshot struct {
	Metadata DeviceMetadata
	Kind     Kind
	Devices  []PairedDevice
}

// Discover scans, opens, and probes all supported receiver candidates. The
// returned receivers own their transports and must be closed by the caller.
func Discover(ctx context.Context, options Options) ([]*Receiver, error) {
	if ctx == nil {
		return nil, errors.New("receiver: nil context")
	}
	var paths []string
	var err error
	if options.Path != "" {
		paths = []string{options.Path}
	} else {
		scanner := options.Scanner
		if scanner == nil {
			scanner = DeviceScanner{}
		}
		paths, err = scanner.Scan()
		if err != nil {
			return nil, err
		}
	}
	opener := options.Opener
	if opener == nil {
		opener = systemOpener(options.SessionOptions)
	}

	result := make([]*Receiver, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		opened, openErr := opener(path)
		if openErr != nil {
			if options.Path != "" {
				return nil, fmt.Errorf("receiver: open %q: %w", path, openErr)
			}
			continue
		}
		if opened.Metadata.Path == "" {
			opened.Metadata.Path = path
		}
		receiver, newErr := New(opened)
		if newErr != nil {
			closeOpened(opened)
			if options.Path != "" {
				return nil, newErr
			}
			continue
		}
		if _, probeErr := receiver.Probe(ctx); probeErr != nil {
			_ = receiver.Close()
			if options.Path != "" {
				return nil, fmt.Errorf("receiver: probe %q: %w", path, probeErr)
			}
			continue
		}
		result = append(result, receiver)
	}
	return result, nil
}

func closeOpened(opened Opened) {
	if opened.Close != nil {
		_ = opened.Close()
	}
}

// OpenAndEnumerate is the lifecycle-shaped one-shot API for callers that need
// one receiver snapshot and do not yet own a daemon lifecycle. It closes every
// opened receiver before returning.
func OpenAndEnumerate(ctx context.Context, options Options) (Snapshot, error) {
	receivers, err := Discover(ctx, options)
	if err != nil {
		return Snapshot{}, err
	}
	if len(receivers) == 0 {
		return Snapshot{}, ErrNoReceiver
	}
	selected := receivers[0]
	for _, other := range receivers[1:] {
		_ = other.Close()
	}
	if err := selected.Configure(ctx); err != nil {
		_ = selected.Close()
		return Snapshot{}, err
	}
	devices, enumErr := selected.EnumeratePaired(ctx)
	closeErr := selected.Close()
	if enumErr != nil {
		return Snapshot{}, enumErr
	}
	if closeErr != nil {
		return Snapshot{}, fmt.Errorf("receiver: close: %w", closeErr)
	}
	return Snapshot{Metadata: selected.Metadata(), Kind: selected.Kind(), Devices: devices}, nil
}

func systemOpener(options hidpp.SessionOptions) Opener {
	return func(path string) (Opened, error) {
		device, err := hidraw.Open(path)
		if err != nil {
			return Opened{}, err
		}
		metadata := DeviceMetadata{Path: path}
		if rawInfo, infoErr := device.GetRawInfo(); infoErr == nil {
			metadata.BusType = rawInfo.BusType
			metadata.VendorID = rawInfo.VendorID
			metadata.ProductID = rawInfo.ProductID
		}
		session, err := hidpp.NewSession(device, options)
		if err != nil {
			_ = device.Close()
			return Opened{}, err
		}
		return Opened{
			Client:   session,
			Reports:  session,
			Metadata: metadata,
			Close:    session.Close,
		}, nil
	}
}
