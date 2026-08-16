// Package uinput contains the portable output contract used by action
// processing and the Linux uinput implementation.
package uinput

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// EventType and event codes intentionally use the values from Linux's input
// UAPI. Keeping these values here makes action processing testable on any
// platform without importing Linux-only packages.
type EventType uint16

const (
	EV_SYN EventType = 0x00
	EV_KEY EventType = 0x01
	EV_REL EventType = 0x02
)

const (
	SYN_REPORT uint16 = 0
)

const (
	REL_X             uint16 = 0x00
	REL_Y             uint16 = 0x01
	REL_HWHEEL        uint16 = 0x06
	REL_WHEEL         uint16 = 0x08
	REL_WHEEL_HI_RES  uint16 = 0x0b
	REL_HWHEEL_HI_RES uint16 = 0x0c
)

// Event is one input event. A call to Output.Sync terminates a logical input
// report with EV_SYN/SYN_REPORT.
type Event struct {
	Type  EventType
	Code  uint16
	Value int32
}

// Output is the small sink required by action processing. Emit and Sync are
// expected to be serialized by the caller; implementations must return errors
// rather than silently dropping a report.
type Output interface {
	Emit(Event) error
	Sync() error
	Close() error
}

// Capabilities is optionally implemented by an Output that can report which
// relative axes were accepted while creating the virtual device.
type Capabilities interface {
	Supports(EventType, uint16) bool
}

// FakeOutput is a hardware-free, concurrency-safe Output implementation. It
// records synchronization events too, so tests can assert complete report
// ordering.
type FakeOutput struct {
	mu sync.Mutex

	Events []Event

	EmitError  error
	SyncError  error
	CloseError error
	closed     bool
	closeErr   error
}

func NewFakeOutput() *FakeOutput { return &FakeOutput{} }

func (f *FakeOutput) Emit(event Event) error {
	if f == nil {
		return errors.New("uinput: nil fake output")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("uinput: fake output is closed")
	}
	if f.EmitError != nil {
		return f.EmitError
	}
	f.Events = append(f.Events, event)
	return nil
}

func (f *FakeOutput) Sync() error {
	if f == nil {
		return errors.New("uinput: nil fake output")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("uinput: fake output is closed")
	}
	if f.SyncError != nil {
		return f.SyncError
	}
	f.Events = append(f.Events, Event{Type: EV_SYN, Code: SYN_REPORT})
	return nil
}

func (f *FakeOutput) Close() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return f.closeErr
	}
	f.closed = true
	f.closeErr = f.CloseError
	return f.closeErr
}

func (f *FakeOutput) Supports(kind EventType, code uint16) bool {
	return f != nil && ((kind == EV_KEY && code <= 0x2ff) || (kind == EV_REL && code <= 0x0f))
}

// Snapshot returns a copy of all events recorded so far.
func (f *FakeOutput) Snapshot() []Event {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Event(nil), f.Events...)
}

// KeyCode parses a Linux key name or a numeric Linux key code. Names are
// deliberately limited to the useful keyboard and mouse controls rather than
// accepting arbitrary host commands.
func KeyCode(name string) (uint16, error) {
	value := strings.ToUpper(strings.TrimSpace(name))
	if value == "" {
		return 0, errors.New("uinput: empty key name")
	}
	if strings.HasPrefix(value, "0X") {
		parsed, err := strconv.ParseUint(value[2:], 16, 16)
		if err != nil {
			return 0, fmt.Errorf("uinput: invalid key code %q: %w", name, err)
		}
		return uint16(parsed), nil
	}
	if parsed, err := strconv.ParseUint(value, 10, 16); err == nil {
		return uint16(parsed), nil
	}
	if code, ok := keyNames[value]; ok {
		return code, nil
	}
	return 0, fmt.Errorf("uinput: unknown key %q", name)
}

// DefaultKeys is the predeclared set used by the Linux device. It includes
// normal keyboard keys, modifiers, navigation, media, and mouse buttons.
var DefaultKeys = func() []uint16 {
	keys := make([]uint16, 0, 128)
	for _, name := range []string{
		"KEY_ESC", "KEY_TAB", "KEY_ENTER", "KEY_BACKSPACE", "KEY_SPACE",
		"KEY_LEFTCTRL", "KEY_RIGHTCTRL", "KEY_LEFTSHIFT", "KEY_RIGHTSHIFT",
		"KEY_LEFTALT", "KEY_RIGHTALT", "KEY_LEFTMETA", "KEY_RIGHTMETA",
		"KEY_CAPSLOCK", "KEY_NUMLOCK", "KEY_SCROLLLOCK", "KEY_COMPOSE",
		"KEY_BACK", "KEY_FORWARD", "KEY_MENU", "KEY_INSERT", "KEY_DELETE",
		"KEY_HOME", "KEY_END", "KEY_PAGEUP", "KEY_PAGEDOWN", "KEY_UP",
		"KEY_DOWN", "KEY_LEFT", "KEY_RIGHT", "KEY_PRINT", "KEY_PAUSE",
		"KEY_VOLUMEUP", "KEY_VOLUMEDOWN", "KEY_MUTE", "KEY_PLAYPAUSE",
		"KEY_NEXTSONG", "KEY_PREVIOUSSONG", "KEY_STOPCD",
		"KEY_F13", "KEY_F14", "KEY_F15", "KEY_F16", "KEY_F17", "KEY_F18",
		"KEY_F19", "KEY_F20", "KEY_F21", "KEY_F22", "KEY_F23", "KEY_F24",
		"KEY_BRIGHTNESSDOWN", "KEY_BRIGHTNESSUP", "KEY_WWW", "KEY_MAIL", "KEY_CALC",
		"BTN_LEFT", "BTN_RIGHT", "BTN_MIDDLE", "BTN_SIDE", "BTN_EXTRA",
		"BTN_FORWARD", "BTN_BACK", "BTN_TASK",
	} {
		code, _ := KeyCode(name)
		keys = append(keys, code)
	}
	for code := uint16(2); code <= 88; code++ {
		keys = append(keys, code)
	}
	return uniqueCodes(keys)
}()

var keyNames = map[string]uint16{
	"KEY_ESC": 1, "ESC": 1,
	"KEY_1": 2, "KEY_2": 3, "KEY_3": 4, "KEY_4": 5, "KEY_5": 6,
	"KEY_6": 7, "KEY_7": 8, "KEY_8": 9, "KEY_9": 10, "KEY_0": 11,
	"KEY_MINUS": 12, "KEY_EQUAL": 13, "KEY_BACKSPACE": 14, "BACKSPACE": 14,
	"KEY_TAB": 15, "TAB": 15,
	"KEY_Q": 16, "KEY_W": 17, "KEY_E": 18, "KEY_R": 19, "KEY_T": 20,
	"KEY_Y": 21, "KEY_U": 22, "KEY_I": 23, "KEY_O": 24, "KEY_P": 25,
	"KEY_LEFTBRACE": 26, "KEY_RIGHTBRACE": 27, "KEY_ENTER": 28, "ENTER": 28,
	"KEY_LEFTCTRL": 29, "CTRL": 29, "CONTROL": 29,
	"KEY_A": 30, "KEY_S": 31, "KEY_D": 32, "KEY_F": 33, "KEY_G": 34,
	"KEY_H": 35, "KEY_J": 36, "KEY_K": 37, "KEY_L": 38,
	"KEY_SEMICOLON": 39, "KEY_APOSTROPHE": 40, "KEY_GRAVE": 41,
	"KEY_LEFTSHIFT": 42, "SHIFT": 42, "KEY_BACKSLASH": 43,
	"KEY_Z": 44, "KEY_X": 45, "KEY_C": 46, "KEY_V": 47, "KEY_B": 48,
	"KEY_N": 49, "KEY_M": 50, "KEY_COMMA": 51, "KEY_DOT": 52, "KEY_SLASH": 53,
	"KEY_RIGHTSHIFT": 54, "KEY_KPASTERISK": 55, "KEY_LEFTALT": 56, "ALT": 56,
	"KEY_SPACE": 57, "SPACE": 57, "KEY_CAPSLOCK": 58,
	"KEY_F1": 59, "KEY_F2": 60, "KEY_F3": 61, "KEY_F4": 62, "KEY_F5": 63,
	"KEY_F6": 64, "KEY_F7": 65, "KEY_F8": 66, "KEY_F9": 67, "KEY_F10": 68,
	"KEY_NUMLOCK": 69, "KEY_SCROLLLOCK": 70, "KEY_F11": 87, "KEY_F12": 88,
	"KEY_F13": 183, "KEY_F14": 184, "KEY_F15": 185, "KEY_F16": 186,
	"KEY_F17": 187, "KEY_F18": 188, "KEY_F19": 189, "KEY_F20": 190,
	"KEY_F21": 191, "KEY_F22": 192, "KEY_F23": 193, "KEY_F24": 194,
	"KEY_RIGHTCTRL": 97, "KEY_RIGHTALT": 100, "KEY_HOME": 102, "HOME": 102,
	"KEY_UP": 103, "UP": 103, "KEY_PAGEUP": 104, "PAGEUP": 104,
	"KEY_LEFT": 105, "LEFT": 105, "KEY_RIGHT": 106, "RIGHT": 106,
	"KEY_END": 107, "END": 107, "KEY_DOWN": 108, "DOWN": 108,
	"KEY_PAGEDOWN": 109, "PAGEDOWN": 109, "KEY_INSERT": 110, "INSERT": 110,
	"KEY_DELETE": 111, "DELETE": 111, "KEY_MUTE": 113, "MUTE": 113,
	"KEY_VOLUMEDOWN": 114, "VOLUMEDOWN": 114, "KEY_VOLUMEUP": 115, "VOLUMEUP": 115,
	"KEY_PAUSE": 119, "PAUSE": 119, "KEY_LEFTMETA": 125, "META": 125, "SUPER": 125,
	"KEY_RIGHTMETA": 126, "KEY_COMPOSE": 127, "KEY_MENU": 139, "MENU": 139,
	"KEY_PREVIOUSSONG": 165, "PREVIOUS": 165, "KEY_PLAYPAUSE": 164, "PLAYPAUSE": 164,
	"KEY_NEXTSONG": 163, "NEXT": 163, "KEY_STOPCD": 166, "STOP": 166,
	"KEY_WWW": 150, "KEY_MAIL": 155, "KEY_CALC": 140, "KEY_BRIGHTNESSDOWN": 224,
	"KEY_BRIGHTNESSUP": 225,
	"KEY_PRINT":        210, "PRINT": 210, "KEY_BACK": 158, "BACK": 158,
	"KEY_FORWARD": 159, "FORWARD": 159,
	"BTN_LEFT": 272, "BUTTON_LEFT": 272, "BTN_RIGHT": 273, "BUTTON_RIGHT": 273,
	"BTN_MIDDLE": 274, "MIDDLE": 274, "BTN_SIDE": 275, "BTN_EXTRA": 276,
	"BTN_FORWARD": 277, "BTN_BACK": 278, "BTN_TASK": 279,
}

func uniqueCodes(values []uint16) []uint16 {
	seen := make(map[uint16]struct{}, len(values))
	result := make([]uint16, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
