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
smart_shift:
  enabled: true
  threshold: 25
  torque: 60
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

func TestLoadRejectsBadCIDsRangesReceiverAndActions(t *testing.T) {
	tests := map[string]string{
		"CID spelling":  "buttons:\n  83:\n    action: back\n",
		"CID width":     "buttons:\n  0x10000:\n    action: back\n",
		"receiver":      "receiver:\n  type: bluetooth\n",
		"index":         "device:\n  index: 7\n",
		"dpi":           "dpi: 99\n",
		"threshold":     "smart_shift:\n  threshold: 51\n",
		"torque":        "smart_shift:\n  torque: 101\n",
		"target":        "hires_scroll:\n  target: nowhere\n",
		"action":        "buttons:\n  0x0053:\n    action: launch-moon\n",
		"missing value": "buttons:\n  0x0053:\n    action: key\n",
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
