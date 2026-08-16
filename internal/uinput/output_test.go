package uinput

import (
	"errors"
	"reflect"
	"testing"
)

func TestFakeOutputRecordsSynchronizedEventsAndErrors(t *testing.T) {
	fake := NewFakeOutput()
	if err := fake.Emit(Event{Type: EV_KEY, Code: 30, Value: 1}); err != nil {
		t.Fatal(err)
	}
	if err := fake.Sync(); err != nil {
		t.Fatal(err)
	}
	want := []Event{{Type: EV_KEY, Code: 30, Value: 1}, {Type: EV_SYN, Code: SYN_REPORT}}
	if got := fake.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	fake.EmitError = errors.New("emit")
	if err := fake.Emit(Event{}); err == nil {
		t.Fatal("Emit unexpectedly succeeded")
	}
	fake.SyncError = errors.New("sync")
	if err := fake.Sync(); err == nil {
		t.Fatal("Sync unexpectedly succeeded")
	}
	if err := fake.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fake.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestKeyCodeAliasesAndNumericCodes(t *testing.T) {
	for name, want := range map[string]uint16{
		"KEY_A": 30, "ctrl": 29, "BTN_MIDDLE": 274, "0x6a": 106,
	} {
		got, err := KeyCode(name)
		if err != nil || got != want {
			t.Fatalf("KeyCode(%q) = %d, %v; want %d", name, got, err, want)
		}
	}
	if _, err := KeyCode("not-a-key"); err == nil {
		t.Fatal("unknown key unexpectedly accepted")
	}
}
