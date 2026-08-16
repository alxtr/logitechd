package mxmaster

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/atremb/logitechd/internal/config"
	"github.com/atremb/logitechd/internal/uinput"
)

func actionSettings(buttons map[config.CID]config.ActionSpec) config.Config {
	return config.Config{Buttons: buttons}
}

func TestActionHandlerKeyPressReleaseOrdering(t *testing.T) {
	output := uinput.NewFakeOutput()
	handler, err := NewActionHandler(actionSettings(map[config.CID]config.ActionSpec{
		0x53: {Action: "key", Value: "KEY_LEFTCTRL+KEY_C+KEY_V"},
	}), output)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(InputEvent{Kind: EventButtons, Buttons: ControlButtonEvent{ControlIDs: []uint16{0x53}}}); err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(InputEvent{Kind: EventButtons}); err != nil {
		t.Fatal(err)
	}
	want := []uinput.Event{
		{Type: uinput.EV_KEY, Code: 29, Value: 1},
		{Type: uinput.EV_KEY, Code: 46, Value: 1},
		{Type: uinput.EV_KEY, Code: 47, Value: 1},
		{Type: uinput.EV_SYN, Code: uinput.SYN_REPORT},
		{Type: uinput.EV_KEY, Code: 47, Value: 0},
		{Type: uinput.EV_KEY, Code: 46, Value: 0},
		{Type: uinput.EV_KEY, Code: 29, Value: 0},
		{Type: uinput.EV_SYN, Code: uinput.SYN_REPORT},
	}
	if got := output.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
}

func TestActionHandlerRelativeAxisAndButtonTransitions(t *testing.T) {
	output := uinput.NewFakeOutput()
	handler, err := NewActionHandler(actionSettings(map[config.CID]config.ActionSpec{
		0x10: {Action: "axis", Value: "REL_X:-4"},
		0x11: {Action: "scroll", Value: "up"},
	}), output)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(InputEvent{Kind: EventButtons, Buttons: ControlButtonEvent{ControlIDs: []uint16{0x10, 0x11}}}); err != nil {
		t.Fatal(err)
	}
	want := []uinput.Event{
		{Type: uinput.EV_REL, Code: uinput.REL_X, Value: -4},
		{Type: uinput.EV_REL, Code: uinput.REL_WHEEL, Value: 1},
		{Type: uinput.EV_SYN, Code: uinput.SYN_REPORT},
	}
	if got := output.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	// Repeating the same button report does not repeat a button action.
	if err := handler.Handle(InputEvent{Kind: EventButtons, Buttons: ControlButtonEvent{ControlIDs: []uint16{0x10, 0x11}}}); err != nil {
		t.Fatal(err)
	}
	if got := len(output.Snapshot()); got != len(want) {
		t.Fatalf("repeat report added %d events", got-len(want))
	}
}

func TestHiResWheelRemainderAndThumbTranslation(t *testing.T) {
	output := uinput.NewFakeOutput()
	enabled := true
	divert := true
	handler, err := NewActionHandler(config.Config{
		HiResScroll: &config.HiResScrollConfig{Enabled: &enabled, Target: "uinput"},
		ThumbWheel:  &config.ThumbWheelConfig{Divert: &divert},
	}, output)
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int16{100, 30} {
		if err := handler.Handle(InputEvent{Kind: EventWheel, Wheel: WheelEvent{HighResolution: true, Delta: delta}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := handler.Handle(InputEvent{Kind: EventThumbWheel, ThumbWheel: ThumbEvent{Delta: -12}}); err != nil {
		t.Fatal(err)
	}
	want := []uinput.Event{
		{Type: uinput.EV_REL, Code: uinput.REL_WHEEL_HI_RES, Value: 100},
		{Type: uinput.EV_SYN, Code: uinput.SYN_REPORT},
		{Type: uinput.EV_REL, Code: uinput.REL_WHEEL_HI_RES, Value: 30},
		{Type: uinput.EV_REL, Code: uinput.REL_WHEEL, Value: 1},
		{Type: uinput.EV_SYN, Code: uinput.SYN_REPORT},
		{Type: uinput.EV_REL, Code: uinput.REL_HWHEEL_HI_RES, Value: -12},
		{Type: uinput.EV_SYN, Code: uinput.SYN_REPORT},
	}
	if got := output.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	var remainder HiResRemainder
	if got := remainder.Add(119); got != 0 || remainder.Remainder() != 119 {
		t.Fatalf("first remainder = %d/%d", got, remainder.Remainder())
	}
	if got := remainder.Add(1); got != 1 || remainder.Remainder() != 0 {
		t.Fatalf("completed remainder = %d/%d", got, remainder.Remainder())
	}
}

func TestRawXYGestureThresholdDirectionReversalAndRelease(t *testing.T) {
	output := uinput.NewFakeOutput()
	handler, err := NewActionHandler(config.Config{Gestures: &config.GestureConfig{
		Threshold: 10,
		Left:      config.ActionSpec{Action: "key", Value: "KEY_A"},
		Right:     config.ActionSpec{Action: "key", Value: "KEY_D"},
	}}, output)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []RawXYEvent{{DX: 5}, {DX: 5}, {DX: -1}, {Release: true}} {
		if err := handler.Handle(InputEvent{Kind: EventRawXY, RawXY: event}); err != nil {
			t.Fatal(err)
		}
	}
	want := []uinput.Event{
		{Type: uinput.EV_KEY, Code: 32, Value: 1},
		{Type: uinput.EV_SYN, Code: uinput.SYN_REPORT},
		{Type: uinput.EV_KEY, Code: 32, Value: 0},
		{Type: uinput.EV_KEY, Code: 30, Value: 1},
		{Type: uinput.EV_SYN, Code: uinput.SYN_REPORT},
		{Type: uinput.EV_KEY, Code: 30, Value: 0},
		{Type: uinput.EV_SYN, Code: uinput.SYN_REPORT},
	}
	if got := output.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("gesture events = %#v, want %#v", got, want)
	}
}

func TestActionHandlerCleanupAndOutputErrors(t *testing.T) {
	output := uinput.NewFakeOutput()
	handler, err := NewActionHandler(actionSettings(map[config.CID]config.ActionSpec{
		1: {Action: "key", Value: "KEY_A"},
	}), output)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(InputEvent{Kind: EventButtons, Buttons: ControlButtonEvent{ControlIDs: []uint16{1}}}); err != nil {
		t.Fatal(err)
	}
	if err := handler.Stop(); err != nil {
		t.Fatal(err)
	}
	if got := output.Snapshot(); len(got) < 4 || got[len(got)-2] != (uinput.Event{Type: uinput.EV_KEY, Code: 30, Value: 0}) {
		t.Fatalf("cleanup events = %#v", got)
	}
	if err := handler.Close(); err == nil {
		// Stop already closed the handler and is idempotent.
	} else {
		t.Fatalf("second close = %v", err)
	}

	failing := uinput.NewFakeOutput()
	failing.EmitError = errors.New("write failed")
	failingHandler, err := NewActionHandler(actionSettings(map[config.CID]config.ActionSpec{
		1: {Action: "key", Value: "KEY_A"},
	}), failing)
	if err != nil {
		t.Fatal(err)
	}
	if err := failingHandler.Handle(InputEvent{Kind: EventButtons, Buttons: ControlButtonEvent{ControlIDs: []uint16{1}}}); err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("error = %v, want output error", err)
	}
}
