// Package mxmaster contains clean-room HID++ clients for the controls used by
// an MX Master 3S. The clients only perform protocol exchanges and decode
// reports; they do not create input devices or execute configured actions.
package mxmaster

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/atremb/logitechd/internal/hidpp"
)

const (
	FeatureSmartShift    uint16 = 0x2110
	FeatureSmartShiftV2  uint16 = 0x2111
	FeatureHiResScroll   uint16 = 0x2120
	FeatureHiResWheel    uint16 = 0x2121
	FeatureThumbWheel    uint16 = 0x2150
	FeatureAdjustableDPI uint16 = 0x2201
	FeatureControlsV1    uint16 = 0x1b00
	FeatureControlsV2    uint16 = 0x1b01
	FeatureControlsV2_2  uint16 = 0x1b02
	FeatureControlsV3    uint16 = 0x1b03
	FeatureControlsV4    uint16 = 0x1b04
	maxDPIListRequests          = 256
)

// FeatureDevice is the small part of a child session used by these clients.
// It also makes all wire-format tests independent of a HIDRAW device.
type FeatureDevice interface {
	LookupFeature(context.Context, uint16) (hidpp.FeatureInfo, error)
	Call(context.Context, byte, byte, []byte) ([]byte, error)
}

// ResponseError indicates that a feature response did not contain the bytes
// required by its documented layout.
type ResponseError struct {
	Feature string
	Need    int
	Got     int
}

func (e *ResponseError) Error() string {
	if e == nil {
		return hidpp.ErrMalformedResponse.Error()
	}
	return fmt.Sprintf("mxmaster: malformed %s response: got %d bytes, need %d", e.Feature, e.Got, e.Need)
}

func (e *ResponseError) Unwrap() error { return hidpp.ErrMalformedResponse }

func need(feature string, data []byte, count int) error {
	if len(data) < count {
		return &ResponseError{Feature: feature, Need: count, Got: len(data)}
	}
	return nil
}

func lookupAny(ctx context.Context, device FeatureDevice, ids ...uint16) (hidpp.FeatureInfo, uint16, error) {
	if device == nil {
		return hidpp.FeatureInfo{}, 0, errors.New("mxmaster: nil feature device")
	}
	var last error
	for _, id := range ids {
		info, err := device.LookupFeature(ctx, id)
		if err == nil {
			return info, id, nil
		}
		if !errors.Is(err, hidpp.ErrUnsupported) {
			return hidpp.FeatureInfo{}, 0, err
		}
		last = err
	}
	if last == nil {
		last = &hidpp.UnsupportedError{Operation: "MX Master feature", Detail: "no feature IDs supplied"}
	}
	return hidpp.FeatureInfo{}, 0, last
}

func call(ctx context.Context, device FeatureDevice, info hidpp.FeatureInfo, function byte, params ...byte) ([]byte, error) {
	return device.Call(ctx, info.Index, function, params)
}

// SmartShiftStatus is the current wheel mode. Threshold is the speed at
// which the wheel changes mode; Torque is only meaningful on the enhanced
// feature variant.
type SmartShiftStatus struct {
	Enabled         bool
	Mode            byte
	Threshold       byte
	Torque          byte
	TorqueSupported bool
}

// SmartShift is a version-aware client for the two Smart Shift feature IDs.
type SmartShift struct {
	device  FeatureDevice
	info    hidpp.FeatureInfo
	feature uint16
	version int
}

func NewSmartShift(ctx context.Context, device FeatureDevice) (*SmartShift, error) {
	info, feature, err := lookupAny(ctx, device, FeatureSmartShiftV2, FeatureSmartShift)
	if err != nil {
		return nil, err
	}
	version := 1
	if feature == FeatureSmartShiftV2 {
		version = 2
	}
	return &SmartShift{device: device, info: info, feature: feature, version: version}, nil
}

func (s *SmartShift) FeatureID() uint16 {
	if s == nil {
		return 0
	}
	return s.feature
}

func (s *SmartShift) Version() int {
	if s == nil {
		return 0
	}
	return s.version
}

func (s *SmartShift) FeatureIndex() byte {
	if s == nil {
		return 0
	}
	return s.info.Index
}

func (s *SmartShift) GetStatus(ctx context.Context) (SmartShiftStatus, error) {
	if s == nil {
		return SmartShiftStatus{}, errors.New("mxmaster: nil smart shift client")
	}
	function := byte(0x00)
	if s.version == 2 {
		function = 0x10
	}
	data, err := call(ctx, s.device, s.info, function)
	if err != nil {
		return SmartShiftStatus{}, err
	}
	if err := need("smart shift status", data, 2); err != nil {
		return SmartShiftStatus{}, err
	}
	status := SmartShiftStatus{
		Mode:      data[0],
		Threshold: data[1],
		Enabled:   data[0] != 1,
	}
	if s.version == 2 {
		capabilities, capErr := call(ctx, s.device, s.info, 0x00)
		if capErr != nil {
			return SmartShiftStatus{}, capErr
		}
		if err := need("smart shift capabilities", capabilities, 1); err != nil {
			return SmartShiftStatus{}, err
		}
		status.TorqueSupported = capabilities[0]&0x01 != 0
		if len(data) >= 3 {
			status.Torque = data[2]
		}
	}
	return status, nil
}

func validThreshold(value byte) bool { return value >= 1 && value <= 50 }
func validTorque(value byte) bool    { return value >= 1 && value <= 100 }

// SetStatus changes the enabled state and threshold while preserving torque
// on enhanced devices. The mode values are the HID++ values: zero selects
// speed-dependent switching and one selects free-spin.
func (s *SmartShift) SetStatus(ctx context.Context, enabled bool, threshold byte) error {
	if s == nil {
		return errors.New("mxmaster: nil smart shift client")
	}
	if !validThreshold(threshold) {
		return fmt.Errorf("mxmaster: smart shift threshold %d is outside 1..50", threshold)
	}
	mode := byte(1)
	if enabled {
		mode = 0
	}
	params := []byte{mode, threshold}
	if s.version == 2 {
		status, err := s.GetStatus(ctx)
		if err != nil {
			return err
		}
		if status.TorqueSupported {
			params = append(params, status.Torque)
		}
	}
	function := byte(0x10)
	if s.version == 2 {
		function = 0x20
	}
	_, err := call(ctx, s.device, s.info, function, params...)
	return err
}

func (s *SmartShift) SetThreshold(ctx context.Context, threshold byte) error {
	status, err := s.GetStatus(ctx)
	if err != nil {
		return err
	}
	return s.SetStatus(ctx, status.Enabled, threshold)
}

func (s *SmartShift) GetThreshold(ctx context.Context) (byte, error) {
	status, err := s.GetStatus(ctx)
	if err != nil {
		return 0, err
	}
	return status.Threshold, nil
}

func (s *SmartShift) GetTorque(ctx context.Context) (byte, error) {
	status, err := s.GetStatus(ctx)
	if err != nil {
		return 0, err
	}
	if s.version != 2 || !status.TorqueSupported {
		return 0, &hidpp.UnsupportedError{Operation: "smart shift torque", Detail: "enhanced torque is not advertised"}
	}
	if !validTorque(status.Torque) {
		return 0, fmt.Errorf("%w: smart shift torque %d is outside 1..100", hidpp.ErrMalformedResponse, status.Torque)
	}
	return status.Torque, nil
}

func (s *SmartShift) SetTorque(ctx context.Context, torque byte) error {
	if !validTorque(torque) {
		return fmt.Errorf("mxmaster: smart shift torque %d is outside 1..100", torque)
	}
	status, err := s.GetStatus(ctx)
	if err != nil {
		return err
	}
	if s.version != 2 || !status.TorqueSupported {
		return &hidpp.UnsupportedError{Operation: "smart shift torque", Detail: "enhanced torque is not advertised"}
	}
	_, err = call(ctx, s.device, s.info, 0x20, statusMode(status), status.Threshold, torque)
	return err
}

func statusMode(status SmartShiftStatus) byte {
	if status.Enabled {
		return 0
	}
	return 1
}

// WheelMode is the bitfield used by the enhanced wheel. UseHIDPP requests
// feature notifications, HighResolution selects fine wheel counts, and
// Invert changes the direction reported by the device.
type WheelMode struct {
	UseHIDPP       bool
	HighResolution bool
	Invert         bool
}

func (m WheelMode) byte() byte {
	var value byte
	if m.UseHIDPP {
		value |= 0x01
	}
	if m.HighResolution {
		value |= 0x02
	}
	if m.Invert {
		value |= 0x04
	}
	return value
}

func wheelMode(value byte) WheelMode {
	return WheelMode{UseHIDPP: value&0x01 != 0, HighResolution: value&0x02 != 0, Invert: value&0x04 != 0}
}

// HiResWheel is a client for the 0x2121 wheel feature, with the older 0x2120
// feature as a protocol fallback.
type HiResWheel struct {
	device  FeatureDevice
	info    hidpp.FeatureInfo
	feature uint16
}

type HiResScroll = HiResWheel

func NewHiResWheel(ctx context.Context, device FeatureDevice) (*HiResWheel, error) {
	info, feature, err := lookupAny(ctx, device, FeatureHiResWheel, FeatureHiResScroll)
	if err != nil {
		return nil, err
	}
	return &HiResWheel{device: device, info: info, feature: feature}, nil
}

func NewHiResScroll(ctx context.Context, device FeatureDevice) (*HiResWheel, error) {
	return NewHiResWheel(ctx, device)
}

func (w *HiResWheel) FeatureID() uint16 {
	if w == nil {
		return 0
	}
	return w.feature
}

func (w *HiResWheel) FeatureIndex() byte {
	if w == nil {
		return 0
	}
	return w.info.Index
}

func (w *HiResWheel) GetCapabilities(ctx context.Context) (byte, error) {
	data, err := call(ctx, w.device, w.info, 0x00)
	if err != nil {
		return 0, err
	}
	if err := need("hi-res wheel capabilities", data, 1); err != nil {
		return 0, err
	}
	return data[0], nil
}

func (w *HiResWheel) GetWheelCapabilities(ctx context.Context) (byte, error) {
	return w.GetCapabilities(ctx)
}

func (w *HiResWheel) GetMode(ctx context.Context) (WheelMode, error) {
	data, err := call(ctx, w.device, w.info, 0x10)
	if err != nil {
		return WheelMode{}, err
	}
	if err := need("hi-res wheel mode", data, 1); err != nil {
		return WheelMode{}, err
	}
	return wheelMode(data[0]), nil
}

func (w *HiResWheel) GetWheelMode(ctx context.Context) (WheelMode, error) {
	return w.GetMode(ctx)
}

func (w *HiResWheel) SetMode(ctx context.Context, mode WheelMode) error {
	_, err := call(ctx, w.device, w.info, 0x20, mode.byte())
	return err
}

func (w *HiResWheel) SetWheelMode(ctx context.Context, mode WheelMode) error {
	return w.SetMode(ctx, mode)
}

// WheelEvent is one unsolicited high-resolution wheel report.
type WheelEvent struct {
	Flags          byte
	Periods        byte
	HighResolution bool
	Delta          int16
}

func DecodeWheelEvent(report hidpp.Report) (WheelEvent, error) {
	if report.Function != 0 {
		return WheelEvent{}, &hidpp.UnsupportedError{Operation: "hi-res wheel event", Detail: fmt.Sprintf("function 0x%x", report.Function)}
	}
	if err := need("hi-res wheel event", report.Parameters, 3); err != nil {
		return WheelEvent{}, err
	}
	return WheelEvent{
		Flags:          report.Parameters[0],
		Periods:        report.Parameters[0] & 0x0f,
		HighResolution: report.Parameters[0]&0x10 != 0,
		Delta:          int16(binary.BigEndian.Uint16(report.Parameters[1:3])),
	}, nil
}

// DPIRange describes the current sensor set and the value used at reset.
type DPIRange struct {
	Sensor  byte
	Current uint16
	Default uint16
}

// AdjustableDPI implements the one-sensor and multi-sensor forms of
// 0x2201. The list decoder understands both literal values and the compact
// step-to-last representation used by HID++ devices.
type AdjustableDPI struct {
	device FeatureDevice
	info   hidpp.FeatureInfo
}

func NewAdjustableDPI(ctx context.Context, device FeatureDevice) (*AdjustableDPI, error) {
	info, _, err := lookupAny(ctx, device, FeatureAdjustableDPI)
	if err != nil {
		return nil, err
	}
	return &AdjustableDPI{device: device, info: info}, nil
}

func (d *AdjustableDPI) SensorCount(ctx context.Context) (byte, error) {
	data, err := call(ctx, d.device, d.info, 0x00)
	if err != nil {
		return 0, err
	}
	if err := need("DPI sensor count", data, 1); err != nil {
		return 0, err
	}
	if data[0] == 0 {
		return 0, fmt.Errorf("%w: DPI sensor count is zero", hidpp.ErrMalformedResponse)
	}
	return data[0], nil
}

func (d *AdjustableDPI) GetSensorCount(ctx context.Context) (byte, error) {
	return d.SensorCount(ctx)
}

func (d *AdjustableDPI) FeatureIndex() byte {
	if d == nil {
		return 0
	}
	return d.info.Index
}

func (d *AdjustableDPI) Supported(ctx context.Context, sensor byte) ([]uint16, error) {
	var encoded []byte
	terminated := false
	for chunk := 0; chunk < maxDPIListRequests; chunk++ {
		data, err := call(ctx, d.device, d.info, 0x10, sensor, 0x00, byte(chunk))
		if err != nil {
			return nil, err
		}
		if err := need("DPI list", data, 2); err != nil {
			return nil, err
		}
		encoded = append(encoded, data[1:]...)
		for offset := 0; offset+1 < len(encoded); offset += 2 {
			if encoded[offset] == 0 && encoded[offset+1] == 0 {
				encoded = encoded[:offset+2]
				terminated = true
				break
			}
		}
		if terminated {
			break
		}
		if len(data) < 16 {
			// A short injected response is a complete frame for tests and
			// transports that do not preserve HID padding.
			break
		}
	}
	if !terminated {
		return nil, &ResponseError{Feature: "DPI list", Need: 2, Got: len(encoded)}
	}
	return decodeDPIList(encoded)
}

func decodeDPIList(data []byte) ([]uint16, error) {
	values := make([]uint16, 0, 16)
	for offset := 0; offset+1 < len(data); offset += 2 {
		value := binary.BigEndian.Uint16(data[offset : offset+2])
		if value == 0 {
			return values, nil
		}
		if value&0xe000 == 0xe000 {
			step := value & 0x1fff
			if step == 0 || offset+3 >= len(data) || len(values) == 0 {
				return nil, &ResponseError{Feature: "DPI list range", Need: offset + 4, Got: len(data)}
			}
			last := binary.BigEndian.Uint16(data[offset+2 : offset+4])
			if last <= values[len(values)-1] {
				return nil, fmt.Errorf("%w: DPI range ends at %d after %d", hidpp.ErrMalformedResponse, last, values[len(values)-1])
			}
			for next := uint32(values[len(values)-1]) + uint32(step); next <= uint32(last); next += uint32(step) {
				values = append(values, uint16(next))
				if len(values) > 4096 {
					return nil, fmt.Errorf("%w: DPI list is unreasonably large", hidpp.ErrMalformedResponse)
				}
			}
			offset += 2
			continue
		}
		values = append(values, value)
		if len(values) > 4096 {
			return nil, fmt.Errorf("%w: DPI list is unreasonably large", hidpp.ErrMalformedResponse)
		}
	}
	if len(data)%2 != 0 {
		return nil, &ResponseError{Feature: "DPI list", Need: len(data) + 1, Got: len(data)}
	}
	return nil, &ResponseError{Feature: "DPI list", Need: 2, Got: len(data)}
}

func (d *AdjustableDPI) Current(ctx context.Context, sensor byte) (DPIRange, error) {
	data, err := call(ctx, d.device, d.info, 0x20, sensor)
	if err != nil {
		return DPIRange{}, err
	}
	if err := need("DPI current", data, 5); err != nil {
		return DPIRange{}, err
	}
	return DPIRange{Sensor: data[0], Current: binary.BigEndian.Uint16(data[1:3]), Default: binary.BigEndian.Uint16(data[3:5])}, nil
}

func (d *AdjustableDPI) GetSensorDPI(ctx context.Context, sensor byte) (DPIRange, error) {
	return d.Current(ctx, sensor)
}

func (d *AdjustableDPI) Set(ctx context.Context, sensor byte, dpi uint16) error {
	if dpi == 0 {
		return fmt.Errorf("mxmaster: DPI must be non-zero")
	}
	_, err := call(ctx, d.device, d.info, 0x30, sensor, byte(dpi>>8), byte(dpi))
	return err
}

func (d *AdjustableDPI) SetSensorDPI(ctx context.Context, sensor byte, dpi uint16) error {
	return d.Set(ctx, sensor, dpi)
}

func (d *AdjustableDPI) GetSensorDPIList(ctx context.Context, sensor byte) ([]uint16, error) {
	return d.Supported(ctx, sensor)
}

func NearestDPI(values []uint16, requested uint16) (uint16, error) {
	if len(values) == 0 {
		return 0, &hidpp.UnsupportedError{Operation: "DPI selection", Detail: "device returned no supported values"}
	}
	best := values[0]
	distance := uint32(absInt(int(requested) - int(best)))
	for _, value := range values[1:] {
		candidateDistance := uint32(absInt(int(requested) - int(value)))
		if candidateDistance < distance || (candidateDistance == distance && value < best) {
			best, distance = value, candidateDistance
		}
	}
	return best, nil
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func (d *AdjustableDPI) SetNearest(ctx context.Context, sensor byte, requested uint16) (uint16, error) {
	values, err := d.Supported(ctx, sensor)
	if err != nil {
		return 0, err
	}
	selected, err := NearestDPI(values, requested)
	if err != nil {
		return 0, err
	}
	if err := d.Set(ctx, sensor, selected); err != nil {
		return 0, err
	}
	return selected, nil
}

// ControlInfo describes one entry in the device's control table.
type ControlInfo struct {
	Index     byte
	CID       uint16
	Task      uint16
	Flags     uint16
	Position  byte
	Group     byte
	GroupMask byte
}

const (
	ControlFlagDivertable = 0x20
	ControlFlagRawXY      = 0x0100
)

type Controls struct {
	device  FeatureDevice
	info    hidpp.FeatureInfo
	feature uint16
}

func NewControls(ctx context.Context, device FeatureDevice) (*Controls, error) {
	info, feature, err := lookupAny(ctx, device, FeatureControlsV4, FeatureControlsV3, FeatureControlsV2_2, FeatureControlsV2, FeatureControlsV1)
	if err != nil {
		return nil, err
	}
	return &Controls{device: device, info: info, feature: feature}, nil
}

func (c *Controls) FeatureID() uint16 {
	if c == nil {
		return 0
	}
	return c.feature
}

func (c *Controls) FeatureIndex() byte {
	if c == nil {
		return 0
	}
	return c.info.Index
}

func (c *Controls) Version() int {
	if c == nil {
		return 0
	}
	return int(c.feature - FeatureControlsV1)
}

func (c *Controls) Count(ctx context.Context) (byte, error) {
	data, err := call(ctx, c.device, c.info, 0x00)
	if err != nil {
		return 0, err
	}
	if err := need("control count", data, 1); err != nil {
		return 0, err
	}
	return data[0], nil
}

func (c *Controls) GetCount(ctx context.Context) (byte, error) { return c.Count(ctx) }

func (c *Controls) Info(ctx context.Context, index byte) (ControlInfo, error) {
	data, err := call(ctx, c.device, c.info, 0x10, index)
	if err != nil {
		return ControlInfo{}, err
	}
	if err := need("control info", data, 5); err != nil {
		return ControlInfo{}, err
	}
	result := ControlInfo{
		Index: index,
		CID:   binary.BigEndian.Uint16(data[0:2]),
		Task:  binary.BigEndian.Uint16(data[2:4]),
		Flags: uint16(data[4]),
	}
	if len(data) >= 9 {
		result.Position = data[5]
		result.Group = data[6]
		result.GroupMask = data[7]
		result.Flags |= uint16(data[8]) << 8
	}
	return result, nil
}

func (c *Controls) GetControlInfo(ctx context.Context, index byte) (ControlInfo, error) {
	return c.Info(ctx, index)
}

type ControlReporting struct {
	CID        uint16
	Diverted   bool
	Persistent bool
	RawXY      bool
	Remap      uint16
}

func decodeReporting(data []byte, feature string) (ControlReporting, error) {
	if err := need(feature, data, 5); err != nil {
		return ControlReporting{}, err
	}
	return ControlReporting{
		CID:        binary.BigEndian.Uint16(data[0:2]),
		Diverted:   data[2]&0x01 != 0,
		Persistent: data[2]&0x04 != 0,
		RawXY:      data[2]&0x10 != 0,
		Remap:      binary.BigEndian.Uint16(data[3:5]),
	}, nil
}

func (c *Controls) Reporting(ctx context.Context, cid uint16) (ControlReporting, error) {
	data, err := call(ctx, c.device, c.info, 0x20, byte(cid>>8), byte(cid))
	if err != nil {
		return ControlReporting{}, err
	}
	return decodeReporting(data, "control reporting")
}

func (c *Controls) GetControlReporting(ctx context.Context, cid uint16) (ControlReporting, error) {
	return c.Reporting(ctx, cid)
}

func (c *Controls) SetTemporaryDiversion(ctx context.Context, cid uint16, diverted bool) (ControlReporting, error) {
	flags := byte(0x02) // d-valid
	if diverted {
		flags |= 0x01
	}
	data, err := call(ctx, c.device, c.info, 0x30, byte(cid>>8), byte(cid), flags, 0x00, 0x00)
	if err != nil {
		return ControlReporting{}, err
	}
	result, err := decodeReporting(data, "control diversion")
	if err != nil {
		return ControlReporting{}, err
	}
	if result.CID != cid {
		return ControlReporting{}, fmt.Errorf("%w: control diversion echoed CID 0x%04x, requested 0x%04x", hidpp.ErrMalformedResponse, result.CID, cid)
	}
	return result, nil
}

// ControlButtonEvent is the list of controls currently pressed in a diverted
// buttons notification.
type ControlButtonEvent struct {
	ControlIDs []uint16
}

func DecodeControlButtonEvent(report hidpp.Report) (ControlButtonEvent, error) {
	if report.Function != 0 {
		return ControlButtonEvent{}, &hidpp.UnsupportedError{Operation: "diverted control event", Detail: fmt.Sprintf("function 0x%x", report.Function)}
	}
	if len(report.Parameters) < 2 {
		return ControlButtonEvent{}, &ResponseError{Feature: "diverted control event", Need: 2, Got: len(report.Parameters)}
	}
	if err := need("diverted control event", report.Parameters, 8); err != nil {
		return ControlButtonEvent{}, err
	}
	limit := len(report.Parameters)
	if limit > 8 {
		limit = 8
	}
	ids := make([]uint16, 0, 4)
	for offset := 0; offset+1 < limit; offset += 2 {
		id := binary.BigEndian.Uint16(report.Parameters[offset : offset+2])
		if id != 0 {
			ids = append(ids, id)
		}
	}
	return ControlButtonEvent{ControlIDs: ids}, nil
}

type RawXYEvent struct {
	DX int16
	DY int16
	// Release is an optional local lifecycle marker. HID++ raw-XY reports do
	// not carry a separate gesture-up bit, so callers may set it when a
	// diversion or child session ends.
	Release bool
}

func DecodeRawXYEvent(report hidpp.Report) (RawXYEvent, error) {
	if report.Function != 1 {
		return RawXYEvent{}, &hidpp.UnsupportedError{Operation: "diverted raw XY event", Detail: fmt.Sprintf("function 0x%x", report.Function)}
	}
	if err := need("diverted raw XY event", report.Parameters, 4); err != nil {
		return RawXYEvent{}, err
	}
	return RawXYEvent{DX: int16(binary.BigEndian.Uint16(report.Parameters[0:2])), DY: int16(binary.BigEndian.Uint16(report.Parameters[2:4]))}, nil
}

// ThumbStatus represents the two independently configurable report flags.
type ThumbStatus struct {
	Diverted bool
	Inverted bool
}

type ThumbWheel struct {
	device FeatureDevice
	info   hidpp.FeatureInfo
}

func NewThumbWheel(ctx context.Context, device FeatureDevice) (*ThumbWheel, error) {
	info, _, err := lookupAny(ctx, device, FeatureThumbWheel)
	if err != nil {
		return nil, err
	}
	return &ThumbWheel{device: device, info: info}, nil
}

func (t *ThumbWheel) Info(ctx context.Context) ([]byte, error) {
	data, err := call(ctx, t.device, t.info, 0x00)
	if err != nil {
		return nil, err
	}
	if err := need("thumb wheel info", data, 1); err != nil {
		return nil, err
	}
	return append([]byte(nil), data...), nil
}

func (t *ThumbWheel) GetInfo(ctx context.Context) ([]byte, error) { return t.Info(ctx) }

func (t *ThumbWheel) FeatureIndex() byte {
	if t == nil {
		return 0
	}
	return t.info.Index
}

func (t *ThumbWheel) Status(ctx context.Context) (ThumbStatus, error) {
	data, err := call(ctx, t.device, t.info, 0x10)
	if err != nil {
		return ThumbStatus{}, err
	}
	if err := need("thumb wheel status", data, 2); err != nil {
		return ThumbStatus{}, err
	}
	return ThumbStatus{Diverted: data[0]&0x01 != 0, Inverted: data[1]&0x01 != 0}, nil
}

func (t *ThumbWheel) GetStatus(ctx context.Context) (ThumbStatus, error) { return t.Status(ctx) }

func (t *ThumbWheel) SetReporting(ctx context.Context, diverted, inverted bool) error {
	params := []byte{0, 0}
	if diverted {
		params[0] = 1
	}
	if inverted {
		params[1] = 1
	}
	_, err := call(ctx, t.device, t.info, 0x20, params...)
	return err
}

func (t *ThumbWheel) SetThumbWheelReporting(ctx context.Context, diverted, inverted bool) error {
	return t.SetReporting(ctx, diverted, inverted)
}

type ThumbEvent struct {
	Delta int16
}

func DecodeThumbWheelEvent(report hidpp.Report) (ThumbEvent, error) {
	if report.Function != 0 {
		return ThumbEvent{}, &hidpp.UnsupportedError{Operation: "thumb wheel event", Detail: fmt.Sprintf("function 0x%x", report.Function)}
	}
	if err := need("thumb wheel event", report.Parameters, 2); err != nil {
		return ThumbEvent{}, err
	}
	return ThumbEvent{Delta: int16(binary.BigEndian.Uint16(report.Parameters[0:2]))}, nil
}

// FeatureSet contains optional clients discovered on one child. Missing
// features are represented by nil pointers and are not errors.
type FeatureSet struct {
	SmartShift *SmartShift
	HiResWheel *HiResWheel
	DPI        *AdjustableDPI
	Controls   *Controls
	ThumbWheel *ThumbWheel
}

func optionalFeature(err error) bool { return errors.Is(err, hidpp.ErrUnsupported) }

func DiscoverFeatures(ctx context.Context, device FeatureDevice) (*FeatureSet, error) {
	result := &FeatureSet{}
	var err error
	if result.SmartShift, err = NewSmartShift(ctx, device); err != nil && !optionalFeature(err) {
		return nil, fmt.Errorf("mxmaster: discover smart shift: %w", err)
	}
	if result.HiResWheel, err = NewHiResWheel(ctx, device); err != nil && !optionalFeature(err) {
		return nil, fmt.Errorf("mxmaster: discover hi-res wheel: %w", err)
	}
	if result.DPI, err = NewAdjustableDPI(ctx, device); err != nil && !optionalFeature(err) {
		return nil, fmt.Errorf("mxmaster: discover DPI: %w", err)
	}
	if result.Controls, err = NewControls(ctx, device); err != nil && !optionalFeature(err) {
		return nil, fmt.Errorf("mxmaster: discover controls: %w", err)
	}
	if result.ThumbWheel, err = NewThumbWheel(ctx, device); err != nil && !optionalFeature(err) {
		return nil, fmt.Errorf("mxmaster: discover thumb wheel: %w", err)
	}
	return result, nil
}
