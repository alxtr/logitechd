package mxmaster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/atremb/logitechd/internal/config"
	"github.com/atremb/logitechd/internal/hidpp"
	"github.com/atremb/logitechd/internal/receiver"
)

var (
	ErrDeviceNotFound = errors.New("mxmaster: configured device was not found")
	ErrNoFeature      = errors.New("mxmaster: requested setting has no supported feature")
)

// SelectChild applies the configured name and/or wireless index to a stable
// child snapshot. A zero index is a wildcard; a blank name means index-only
// selection. The default name is applied when target is empty.
func SelectChild(children []*receiver.ChildDevice, target config.DeviceConfig) (*receiver.ChildDevice, error) {
	name := target.Name
	if name == "" && target.Index == 0 {
		name = config.DefaultDeviceName
	}
	for _, child := range children {
		if child == nil {
			continue
		}
		metadata := child.Metadata()
		if name != "" && metadata.Name != name {
			continue
		}
		if target.Index != 0 && metadata.WirelessIndex != target.Index {
			continue
		}
		return child, nil
	}
	criteria := name
	if criteria == "" {
		criteria = fmt.Sprintf("index %d", target.Index)
	} else if target.Index != 0 {
		criteria = fmt.Sprintf("%q at index %d", criteria, target.Index)
	}
	return nil, fmt.Errorf("%w: %s", ErrDeviceNotFound, criteria)
}

// SelectMetadata is the hardware-free form of SelectChild. It is useful to
// select a target before child handles have been attached to a live session.
func SelectMetadata(children []receiver.ChildMetadata, target config.DeviceConfig) (receiver.ChildMetadata, error) {
	name := target.Name
	if name == "" && target.Index == 0 {
		name = config.DefaultDeviceName
	}
	for _, child := range children {
		if name != "" && child.Name != name {
			continue
		}
		if target.Index != 0 && child.WirelessIndex != target.Index {
			continue
		}
		return child, nil
	}
	criteria := name
	if criteria == "" {
		criteria = fmt.Sprintf("index %d", target.Index)
	} else if target.Index != 0 {
		criteria = fmt.Sprintf("%q at index %d", criteria, target.Index)
	}
	return receiver.ChildMetadata{}, fmt.Errorf("%w: %s", ErrDeviceNotFound, criteria)
}

type EventKind uint8

const (
	EventWheel EventKind = iota + 1
	EventThumbWheel
	EventButtons
	EventRawXY
)

func (k EventKind) String() string {
	switch k {
	case EventWheel:
		return "wheel"
	case EventThumbWheel:
		return "thumb-wheel"
	case EventButtons:
		return "buttons"
	case EventRawXY:
		return "raw-xy"
	default:
		return "unknown"
	}
}

// InputEvent is the decoded, still device-independent input surface exposed
// to Phase 6. No event is written to uinput here.
type InputEvent struct {
	Kind       EventKind
	Wheel      WheelEvent
	ThumbWheel ThumbEvent
	Buttons    ControlButtonEvent
	RawXY      RawXYEvent
}

// Configurator owns the selected child and its optional feature clients.
// Close stops event delivery but never closes the receiver session.
type Configurator struct {
	child    *receiver.ChildDevice
	settings config.Config
	features *FeatureSet

	events chan InputEvent
	unsub  func()

	eventMu   sync.RWMutex
	closeOnce sync.Once
}

// NewConfigurator selects a child, discovers optional features, and starts a
// decoded event stream. It does not apply settings until Apply is called.
func NewConfigurator(ctx context.Context, session *receiver.ReceiverSession, settings config.Config) (*Configurator, error) {
	if ctx == nil {
		return nil, errors.New("mxmaster: nil configurator context")
	}
	if session == nil {
		return nil, errors.New("mxmaster: nil receiver session")
	}
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	child, err := SelectChild(session.Children(), settings.Device)
	if err != nil {
		return nil, err
	}
	if child.Client() == nil {
		return nil, errors.New("mxmaster: selected child has no HID++ session")
	}
	features, err := DiscoverFeatures(ctx, child.Client())
	if err != nil {
		return nil, err
	}
	result := &Configurator{
		child:    child,
		settings: settings,
		features: features,
		events:   make(chan InputEvent, 64),
	}
	result.unsub = child.Client().SubscribeEvents(result.route)
	return result, nil
}

// Configure is an explicit spelling for NewConfigurator.
func Configure(ctx context.Context, session *receiver.ReceiverSession, settings config.Config) (*Configurator, error) {
	return NewConfigurator(ctx, session, settings)
}

func (c *Configurator) Child() *receiver.ChildDevice {
	if c == nil {
		return nil
	}
	return c.child
}

func (c *Configurator) Features() *FeatureSet {
	if c == nil {
		return nil
	}
	return c.features
}

func (c *Configurator) Events() <-chan InputEvent {
	if c == nil {
		return nil
	}
	return c.events
}

// StartActions attaches a serialized Phase 6 action handler to this
// configurator's decoded event stream. The caller owns the returned handler's
// lifecycle and must stop it before closing the underlying output.
func (c *Configurator) StartActions(ctx context.Context, output Output) (*ActionHandler, error) {
	if c == nil {
		return nil, errors.New("mxmaster: nil configurator")
	}
	handler, err := NewActionHandler(c.settings, output)
	if err != nil {
		return nil, err
	}
	if err := handler.Start(ctx, c.Events()); err != nil {
		_ = handler.Close()
		return nil, err
	}
	return handler, nil
}

func (c *Configurator) route(report hidpp.Report) {
	if c == nil || c.features == nil {
		return
	}
	c.eventMu.RLock()
	defer c.eventMu.RUnlock()
	var event InputEvent
	var ok bool
	switch {
	case c.features.HiResWheel != nil && report.SubID == c.features.HiResWheel.FeatureIndex():
		decoded, err := DecodeWheelEvent(report)
		if err == nil {
			event = InputEvent{Kind: EventWheel, Wheel: decoded}
			ok = true
		}
	case c.features.ThumbWheel != nil && report.SubID == c.features.ThumbWheel.FeatureIndex():
		decoded, err := DecodeThumbWheelEvent(report)
		if err == nil {
			event = InputEvent{Kind: EventThumbWheel, ThumbWheel: decoded}
			ok = true
		}
	case c.features.Controls != nil && report.SubID == c.features.Controls.FeatureIndex():
		switch report.Function {
		case 0:
			decoded, err := DecodeControlButtonEvent(report)
			if err == nil {
				event = InputEvent{Kind: EventButtons, Buttons: decoded}
				ok = true
			}
		case 1:
			decoded, err := DecodeRawXYEvent(report)
			if err == nil {
				event = InputEvent{Kind: EventRawXY, RawXY: decoded}
				ok = true
			}
		}
	}
	if !ok {
		return
	}
	select {
	case c.events <- event:
	default:
		// Input reports must not block the shared HID++ reader. Phase 6 can
		// choose a larger queue or a back-pressure policy around this stream.
	}
}

// Apply sends only the feature settings present in the configuration. Missing
// optional features are reported as a typed unsupported error rather than
// being silently treated as success.
func (c *Configurator) Apply(ctx context.Context) error {
	if c == nil || c.features == nil {
		return errors.New("mxmaster: nil configurator")
	}
	if err := c.applySmartShift(ctx); err != nil {
		return err
	}
	if err := c.applyWheel(ctx); err != nil {
		return err
	}
	if err := c.applyDPI(ctx); err != nil {
		return err
	}
	if err := c.applyThumbWheel(ctx); err != nil {
		return err
	}
	if err := c.applyButtons(ctx); err != nil {
		return err
	}
	return nil
}

func (c *Configurator) applySmartShift(ctx context.Context) error {
	setting := c.settings.SmartShift
	if setting == nil {
		return nil
	}
	if c.features.SmartShift == nil {
		return fmt.Errorf("%w: smart shift", ErrNoFeature)
	}
	if setting.Enabled != nil || setting.Threshold != nil {
		status, err := c.features.SmartShift.GetStatus(ctx)
		if err != nil {
			return fmt.Errorf("mxmaster: read smart shift: %w", err)
		}
		enabled, threshold := status.Enabled, status.Threshold
		if setting.Enabled != nil {
			enabled = *setting.Enabled
		}
		if setting.Threshold != nil {
			threshold = byte(*setting.Threshold)
		}
		if err := c.features.SmartShift.SetStatus(ctx, enabled, threshold); err != nil {
			return fmt.Errorf("mxmaster: set smart shift: %w", err)
		}
	}
	if setting.Torque != nil {
		if err := c.features.SmartShift.SetTorque(ctx, byte(*setting.Torque)); err != nil {
			return fmt.Errorf("mxmaster: set smart shift torque: %w", err)
		}
	}
	return nil
}

func (c *Configurator) applyWheel(ctx context.Context) error {
	setting := c.settings.HiResScroll
	if setting == nil {
		return nil
	}
	if c.features.HiResWheel == nil {
		return fmt.Errorf("%w: hi-res wheel", ErrNoFeature)
	}
	mode, err := c.features.HiResWheel.GetMode(ctx)
	if err != nil {
		return fmt.Errorf("mxmaster: read hi-res wheel: %w", err)
	}
	if setting.Enabled != nil {
		mode.UseHIDPP = *setting.Enabled
		mode.HighResolution = *setting.Enabled
	}
	if setting.Invert != nil {
		mode.Invert = *setting.Invert
	}
	if setting.Enabled != nil || setting.Invert != nil {
		if err := c.features.HiResWheel.SetMode(ctx, mode); err != nil {
			return fmt.Errorf("mxmaster: set hi-res wheel: %w", err)
		}
	}
	return nil
}

func (c *Configurator) applyDPI(ctx context.Context) error {
	if c.settings.DPI == nil {
		return nil
	}
	if c.features.DPI == nil {
		return fmt.Errorf("%w: adjustable DPI", ErrNoFeature)
	}
	selected, err := c.features.DPI.SetNearest(ctx, 0, uint16(*c.settings.DPI))
	if err != nil {
		return fmt.Errorf("mxmaster: set DPI: %w", err)
	}
	_ = selected
	return nil
}

func (c *Configurator) applyThumbWheel(ctx context.Context) error {
	setting := c.settings.ThumbWheel
	if setting == nil {
		return nil
	}
	if c.features.ThumbWheel == nil {
		return fmt.Errorf("%w: thumb wheel", ErrNoFeature)
	}
	status, err := c.features.ThumbWheel.Status(ctx)
	if err != nil {
		return fmt.Errorf("mxmaster: read thumb wheel: %w", err)
	}
	if setting.Divert != nil {
		status.Diverted = *setting.Divert
	}
	if setting.Invert != nil {
		status.Inverted = *setting.Invert
	}
	if setting.Divert != nil || setting.Invert != nil {
		if err := c.features.ThumbWheel.SetReporting(ctx, status.Diverted, status.Inverted); err != nil {
			return fmt.Errorf("mxmaster: set thumb wheel: %w", err)
		}
	}
	return nil
}

func (c *Configurator) applyButtons(ctx context.Context) error {
	if len(c.settings.Buttons) == 0 {
		return nil
	}
	if c.features.Controls == nil {
		return fmt.Errorf("%w: reprogrammable controls", ErrNoFeature)
	}
	for cid, action := range c.settings.Buttons {
		diverted := !strings.EqualFold(strings.TrimSpace(action.Action), "none")
		if _, err := c.features.Controls.SetTemporaryDiversion(ctx, uint16(cid), diverted); err != nil {
			return fmt.Errorf("mxmaster: configure button %s: %w", cid, err)
		}
	}
	return nil
}

func (c *Configurator) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.eventMu.Lock()
		defer c.eventMu.Unlock()
		if c.unsub != nil {
			c.unsub()
		}
		close(c.events)
	})
	return nil
}
