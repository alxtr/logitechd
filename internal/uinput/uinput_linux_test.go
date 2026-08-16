//go:build linux

package uinput

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestInputEventMatchesNativeLinuxLayout(t *testing.T) {
	wantSize := unsafe.Sizeof(unix.Timeval{}) + unsafe.Sizeof(uint16(0)) + unsafe.Sizeof(uint16(0)) + unsafe.Sizeof(int32(0))
	if got := unsafe.Sizeof(inputEvent{}); got != wantSize {
		t.Fatalf("input_event size = %d, want native size %d", got, wantSize)
	}
	if got := unsafe.Offsetof(inputEvent{}.Type); got != unsafe.Sizeof(unix.Timeval{}) {
		t.Fatalf("input_event.Type offset = %d, want %d", got, unsafe.Sizeof(unix.Timeval{}))
	}
	if got := unsafe.Offsetof(inputEvent{}.Code); got != unsafe.Sizeof(unix.Timeval{})+2 {
		t.Fatalf("input_event.Code offset = %d, want %d", got, unsafe.Sizeof(unix.Timeval{})+2)
	}
	if got := unsafe.Offsetof(inputEvent{}.Value); got != unsafe.Sizeof(unix.Timeval{})+4 {
		t.Fatalf("input_event.Value offset = %d, want %d", got, unsafe.Sizeof(unix.Timeval{})+4)
	}
	if unsafe.Sizeof(uintptr(0)) == 8 && unsafe.Sizeof(inputEvent{}) != 24 {
		t.Fatalf("amd64 input_event size = %d, want 24", unsafe.Sizeof(inputEvent{}))
	}
}
