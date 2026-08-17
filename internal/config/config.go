// Package config defines the deliberately small configuration language used
// by logitechd. It is not intended to read or write any other Logitech tool's
// configuration format.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	DefaultDeviceName = "MX Master 3S"
	MaxWirelessIndex  = 6
	MinDPI            = 100
	MaxDPI            = 8000
)

var (
	ErrInvalidConfig = errors.New("config: invalid configuration")
	ErrInvalidCID    = errors.New("config: invalid control ID")
)

// ReceiverType identifies the wireless receiver family to probe.
type ReceiverType string

const (
	ReceiverBolt     ReceiverType = "bolt"
	ReceiverUnifying ReceiverType = "unifying"
)

// ReceiverConfig selects the receiver. Path is optional; an empty path asks
// the receiver scanner to inspect the normal HIDRAW candidates.
type ReceiverConfig struct {
	Path string       `yaml:"path"`
	Type ReceiverType `yaml:"type"`
}

// DeviceConfig selects a child. Name and Index are independently optional;
// when neither is specified the MX Master 3S name is used.
type DeviceConfig struct {
	Name  string `yaml:"name"`
	Index uint8  `yaml:"index"`
}

// SmartShiftConfig contains the optional wheel tuning settings. Threshold is
// in the device's 1..255 byte scale: 1..254 enables speed-based disengagement
// and 255 disables it. Torque is in the enhanced feature's 1..100 scale.
type SmartShiftConfig struct {
	Enabled   *bool `yaml:"enabled"`
	Threshold *int  `yaml:"threshold"`
	Torque    *int  `yaml:"torque"`
}

// HiResScrollConfig controls high-resolution wheel reporting. Target is the
// destination for decoded wheel events.
type HiResScrollConfig struct {
	Enabled *bool  `yaml:"enabled"`
	Invert  *bool  `yaml:"invert"`
	Target  string `yaml:"target"`
}

// ThumbWheelConfig controls thumb-wheel diversion and direction.
type ThumbWheelConfig struct {
	Divert *bool `yaml:"divert"`
	Invert *bool `yaml:"invert"`
}

// ActionSpec is a declarative button or gesture action. The action field is one
// of the names accepted by Validate; Value is used by actions that need an
// argument.
type ActionSpec struct {
	Action string `yaml:"action"`
	Value  string `yaml:"value"`
}

// GestureConfig describes actions held while a diverted raw-XY gesture is
// moving in one of the four cardinal directions. An omitted action means that
// direction is ignored. Threshold is measured in accumulated device movement
// units and defaults to a conservative value when omitted.
type GestureConfig struct {
	Threshold int        `yaml:"threshold"`
	Left      ActionSpec `yaml:"left"`
	Right     ActionSpec `yaml:"right"`
	Up        ActionSpec `yaml:"up"`
	Down      ActionSpec `yaml:"down"`
}

// RawXYConfig is an explicit name for GestureConfig used by callers that keep
// the HID++ event name in their configuration model.
type RawXYConfig = GestureConfig

// UnmarshalYAML also permits the compact form `0x0053: back`. Both forms are
// part of this configuration language; mapping fields remain explicitly
// checked here because a custom unmarshaller bypasses yaml.Decoder's
// KnownFields handling for its contents.
func (a *ActionSpec) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		return fmt.Errorf("action specification is missing")
	}
	if node.Kind == yaml.ScalarNode {
		if node.Tag != "!!str" {
			return fmt.Errorf("action specification must be a string or mapping")
		}
		a.Action = node.Value
		a.Value = ""
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("action specification must be a string or mapping")
	}
	for offset := 0; offset+1 < len(node.Content); offset += 2 {
		field := node.Content[offset].Value
		switch field {
		case "action", "value":
		default:
			return fmt.Errorf("unknown action field %q", field)
		}
	}
	type plain ActionSpec
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*a = ActionSpec(decoded)
	return nil
}

// CID is a 16-bit HID++ control identifier. YAML keys must use a 0x-prefixed
// hexadecimal spelling so a decimal typo cannot silently select another
// control.
type CID uint16

func (c CID) String() string { return fmt.Sprintf("0x%04x", uint16(c)) }

func (c *CID) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.ScalarNode {
		return fmt.Errorf("%w: CID must be a scalar", ErrInvalidCID)
	}
	value := strings.TrimSpace(node.Value)
	if len(value) < 3 || !(strings.HasPrefix(value, "0x") || strings.HasPrefix(value, "0X")) {
		return fmt.Errorf("%w: %q must use 0x-prefixed hexadecimal", ErrInvalidCID, value)
	}
	hex := value[2:]
	if len(hex) > 4 || hex == "" {
		return fmt.Errorf("%w: %q is outside 16-bit range", ErrInvalidCID, value)
	}
	parsed, err := strconv.ParseUint(hex, 16, 16)
	if err != nil {
		return fmt.Errorf("%w: %q: %w", ErrInvalidCID, value, err)
	}
	*c = CID(parsed)
	return nil
}

// Config is the complete configuration.
type Config struct {
	Receiver    ReceiverConfig     `yaml:"receiver"`
	Device      DeviceConfig       `yaml:"device"`
	DPI         *int               `yaml:"dpi"`
	SmartShift  *SmartShiftConfig  `yaml:"smart_shift"`
	HiResScroll *HiResScrollConfig `yaml:"hires_scroll"`
	ThumbWheel  *ThumbWheelConfig  `yaml:"thumb_wheel"`
	Buttons     map[CID]ActionSpec `yaml:"buttons"`
	Gestures    *GestureConfig     `yaml:"gestures"`
	RawXY       *RawXYConfig       `yaml:"raw_xy"`
}

// LoadFile reads and validates one YAML document.
func LoadFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: read %q: %w", path, err)
	}
	config, err := Load(data)
	if err != nil {
		return Config{}, fmt.Errorf("config: load %q: %w", path, err)
	}
	return config, nil
}

// Load decodes one strict YAML document and validates it. An empty document
// is treated as an empty configuration, which is useful for defaults.
func Load(data []byte) (Config, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var value Config
	if err := decoder.Decode(&value); err != nil {
		if err == io.EOF {
			value = Config{}
		} else {
			return Config{}, fmt.Errorf("%w: YAML: %w", ErrInvalidConfig, err)
		}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Config{}, fmt.Errorf("%w: multiple YAML documents are not allowed", ErrInvalidConfig)
		}
		return Config{}, fmt.Errorf("%w: trailing YAML: %w", ErrInvalidConfig, err)
	}
	if err := value.Validate(); err != nil {
		return Config{}, err
	}
	return value, nil
}

// LoadYAML is an explicit alias for callers whose source is already named as
// YAML.
func LoadYAML(data []byte) (Config, error) { return Load(data) }

// Validate applies defaults and checks all values that can be checked without
// talking to a device.
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("%w: nil configuration", ErrInvalidConfig)
	}
	if c.Receiver.Type == "" {
		c.Receiver.Type = ReceiverBolt
	}
	switch c.Receiver.Type {
	case ReceiverBolt, ReceiverUnifying:
	default:
		return fmt.Errorf("%w: receiver.type %q is not bolt or unifying", ErrInvalidConfig, c.Receiver.Type)
	}
	if c.Receiver.Path != "" {
		if strings.TrimSpace(c.Receiver.Path) != c.Receiver.Path || !strings.HasPrefix(c.Receiver.Path, "/") {
			return fmt.Errorf("%w: receiver.path %q must be an absolute HIDRAW path", ErrInvalidConfig, c.Receiver.Path)
		}
	}
	if c.Device.Name == "" && c.Device.Index == 0 {
		c.Device.Name = DefaultDeviceName
	}
	if c.Device.Name != "" && strings.TrimSpace(c.Device.Name) == "" {
		return fmt.Errorf("%w: device.name cannot be blank", ErrInvalidConfig)
	}
	if c.Device.Index > MaxWirelessIndex {
		return fmt.Errorf("%w: device.index %d is outside 1..%d", ErrInvalidConfig, c.Device.Index, MaxWirelessIndex)
	}
	if c.DPI != nil && (*c.DPI < MinDPI || *c.DPI > MaxDPI) {
		return fmt.Errorf("%w: dpi %d is outside %d..%d", ErrInvalidConfig, *c.DPI, MinDPI, MaxDPI)
	}
	if c.SmartShift != nil {
		if c.SmartShift.Threshold != nil && (*c.SmartShift.Threshold < 1 || *c.SmartShift.Threshold > 255) {
			return fmt.Errorf("%w: smart_shift.threshold %d is outside 1..255", ErrInvalidConfig, *c.SmartShift.Threshold)
		}
		if c.SmartShift.Torque != nil && (*c.SmartShift.Torque < 1 || *c.SmartShift.Torque > 100) {
			return fmt.Errorf("%w: smart_shift.torque %d is outside 1..100", ErrInvalidConfig, *c.SmartShift.Torque)
		}
		if c.SmartShift.Enabled != nil && !*c.SmartShift.Enabled && c.SmartShift.Threshold != nil {
			return fmt.Errorf("%w: smart_shift.threshold cannot be set when smart_shift.enabled is false", ErrInvalidConfig)
		}
	}
	if c.HiResScroll != nil {
		if c.HiResScroll.Target == "" {
			c.HiResScroll.Target = "host"
		}
		switch c.HiResScroll.Target {
		case "host", "device", "hidpp", "os", "uinput":
		default:
			return fmt.Errorf("%w: hires_scroll.target %q is not host, device, hidpp, os, or uinput", ErrInvalidConfig, c.HiResScroll.Target)
		}
	}
	for cid, action := range c.Buttons {
		if err := validateAction(cid, action); err != nil {
			return err
		}
	}
	for name, gestures := range map[string]*GestureConfig{"gestures": c.Gestures, "raw_xy": c.RawXY} {
		if gestures == nil {
			continue
		}
		if gestures.Threshold < 0 || gestures.Threshold > 32767 {
			return fmt.Errorf("%w: %s.threshold %d is outside 0..32767", ErrInvalidConfig, name, gestures.Threshold)
		}
		for direction, action := range map[string]ActionSpec{
			"left": gestures.Left, "right": gestures.Right,
			"up": gestures.Up, "down": gestures.Down,
		} {
			if action.Action == "" {
				continue
			}
			if err := validateActionName(name+"."+direction, action); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateAction(cid CID, action ActionSpec) error {
	return validateActionName("buttons."+cid.String(), action)
}

func validateActionName(location string, action ActionSpec) error {
	name := strings.ToLower(strings.TrimSpace(action.Action))
	switch name {
	case "none", "back", "forward", "middle", "copy", "paste":
		if action.Value != "" {
			return fmt.Errorf("%w: %s action %q does not accept value", ErrInvalidConfig, location, name)
		}
	case "key", "button", "command", "scroll", "axis", "relative":
		if strings.TrimSpace(action.Value) == "" {
			return fmt.Errorf("%w: %s action %q requires value", ErrInvalidConfig, location, name)
		}
		if name == "scroll" {
			switch strings.ToLower(action.Value) {
			case "up", "down", "left", "right":
			default:
				return fmt.Errorf("%w: %s scroll value %q is invalid", ErrInvalidConfig, location, action.Value)
			}
		}
	default:
		return fmt.Errorf("%w: %s action %q is invalid", ErrInvalidConfig, location, action.Action)
	}
	return nil
}
