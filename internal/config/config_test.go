package config

import (
	"errors"
	"testing"
)

func TestLoadStrictConfigurationAndDefaults(t *testing.T) {
	data := []byte(`
receiver:
  path: /dev/hidraw-test
  type: bolt
device:
  name: MX Master 3S
  index: 2
dpi: 1600
scroll_mode: ratchet
smart_shift:
  threshold: 100
  torque: 100
hires_scroll:
  enabled: true
  invert: false
  target: host
thumb_wheel:
  divert: true
  invert: true
buttons:
  0x00c3:
    action: key
    value: KEY_LEFTMETA
  0x0053:
    action: scroll
    value: left
`)
	value, err := Load(data)
	if err != nil {
		t.Fatal(err)
	}
	if value.Receiver.Type != ReceiverBolt || value.Device.Index != 2 || value.DPI == nil || *value.DPI != 1600 {
		t.Fatalf("decoded config = %+v", value)
	}
	if value.ScrollMode == nil || *value.ScrollMode != ScrollModeRatchet {
		t.Fatalf("decoded scroll mode = %v", value.ScrollMode)
	}
	if value.SmartShift == nil || value.SmartShift.Threshold == nil || *value.SmartShift.Threshold != 100 || value.SmartShift.Torque == nil || *value.SmartShift.Torque != 100 {
		t.Fatalf("decoded smart shift = %+v", value.SmartShift)
	}
	if len(value.Buttons) != 2 || value.Buttons[CID(0x00c3)].Value != "KEY_LEFTMETA" {
		t.Fatalf("decoded buttons = %+v", value.Buttons)
	}

	defaults, err := Load([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Receiver.Type != ReceiverBolt || defaults.Device.Name != DefaultDeviceName {
		t.Fatalf("defaults = %+v", defaults)
	}
}

func TestLoadRejectsUnknownFieldsAndMultipleDocuments(t *testing.T) {
	for name, data := range map[string]string{
		"root":     "unknown: true\n",
		"nested":   "smart_shift:\n  threshold: 10\n  unknown: true\n",
		"enabled":  "smart_shift:\n  enabled: true\n",
		"action":   "buttons:\n  0x0053:\n    action: key\n    unknown: A\n",
		"multiple": "{}\n---\n{}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load([]byte(data)); err == nil || !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Load() error = %v, want invalid config", err)
			}
		})
	}
}

func TestLoadValidatesScrollModes(t *testing.T) {
	for _, mode := range []ScrollMode{ScrollModeSmartShift, ScrollModeFreeSpin, ScrollModeRatchet} {
		t.Run(string(mode), func(t *testing.T) {
			value, err := Load([]byte("scroll_mode: " + string(mode) + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			if value.ScrollMode == nil || *value.ScrollMode != mode {
				t.Fatalf("scroll mode = %v, want %q", value.ScrollMode, mode)
			}
		})
	}

	for _, mode := range []string{"", "smartshift", "free-spin", "automatic", "3"} {
		t.Run("reject_"+mode, func(t *testing.T) {
			if _, err := Load([]byte("scroll_mode: \"" + mode + "\"\n")); err == nil || !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Load() error = %v, want invalid config", err)
			}
		})
	}
}

func TestLoadRejectsBadCIDsRangesReceiverAndActions(t *testing.T) {
	tests := map[string]string{
		"CID spelling":   "buttons:\n  83:\n    action: back\n",
		"CID width":      "buttons:\n  0x10000:\n    action: back\n",
		"receiver":       "receiver:\n  type: bluetooth\n",
		"index":          "device:\n  index: 7\n",
		"dpi":            "dpi: 99\n",
		"threshold zero": "smart_shift:\n  threshold: 0\n",
		"threshold byte": "smart_shift:\n  threshold: 256\n",
		"torque":         "smart_shift:\n  torque: 101\n",
		"target":         "hires_scroll:\n  target: nowhere\n",
		"action":         "buttons:\n  0x0053:\n    action: launch-moon\n",
		"missing value":  "buttons:\n  0x0053:\n    action: key\n",
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Load([]byte(data)); err == nil || !errors.Is(err, ErrInvalidConfig) && !errors.Is(err, ErrInvalidCID) {
				t.Fatalf("Load() error = %v, want validation error", err)
			}
		})
	}
}

func TestValidateCanBeUsedWithProgrammaticValues(t *testing.T) {
	value := Config{Buttons: map[CID]ActionSpec{CID(0x53): {Action: "none"}}}
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	if value.Device.Name != DefaultDeviceName {
		t.Fatalf("device default = %q", value.Device.Name)
	}
}

func TestLoadAcceptsCompactNamedButtonAction(t *testing.T) {
	value, err := Load([]byte("buttons:\n  0x0053: back\n"))
	if err != nil {
		t.Fatal(err)
	}
	if value.Buttons[CID(0x53)].Action != "back" {
		t.Fatalf("compact action = %+v", value.Buttons)
	}
}

func TestLoadValidatesPhaseSixActionsAndGestures(t *testing.T) {
	value, err := Load([]byte(`
hires_scroll:
  enabled: true
  target: uinput
gestures:
  threshold: 12
  left:
    action: key
    value: KEY_LEFTCTRL+KEY_A
  right:
    action: button
    value: BTN_RIGHT
  down:
    action: axis
    value: REL_Y:4
buttons:
  0x0053:
    action: relative
    value: REL_X:-2
`))
	if err != nil {
		t.Fatal(err)
	}
	if value.Gestures == nil || value.Gestures.Threshold != 12 || value.HiResScroll.Target != "uinput" {
		t.Fatalf("phase six configuration = %+v", value)
	}

	for _, data := range []string{
		"gestures:\n  threshold: 32768\n",
		"gestures:\n  left:\n    action: key\n",
		"buttons:\n  0x0053:\n    action: axis\n",
	} {
		if _, err := Load([]byte(data)); err == nil || !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("Load(%q) error = %v, want invalid config", data, err)
		}
	}
}
