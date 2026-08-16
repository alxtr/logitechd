//go:build linux

package uinput

import (
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestCapabilityIoctlsPassValuesDirectly(t *testing.T) {
	type call struct {
		fd       int
		request  uintptr
		argument uintptr
	}
	var calls []call
	originalInvokeIoctl := invokeIoctl
	t.Cleanup(func() { invokeIoctl = originalInvokeIoctl })
	invokeIoctl = func(fd int, request uintptr, argument uintptr) error {
		calls = append(calls, call{fd: fd, request: request, argument: argument})
		return nil
	}

	device := &Device{file: nil}
	device.file = fakeUinputFile(37)
	t.Cleanup(func() { device.file = nil })

	tests := []struct {
		name    string
		request uintptr
		value   int
		call    func() error
	}{
		{name: "event", request: uiSetEvBit, value: int(EV_REL), call: func() error {
			return device.setEventBit(EV_REL)
		}},
		{name: "key", request: uiSetKeyBit, value: 274, call: func() error {
			return device.setKeyBit(274)
		}},
		{name: "relative axis", request: uiSetRelBit, value: int(REL_WHEEL_HI_RES), call: func() error {
			return device.setRelBit(REL_WHEEL_HI_RES)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls = nil
			if err := test.call(); err != nil {
				t.Fatal(err)
			}
			if len(calls) != 1 {
				t.Fatalf("ioctl calls = %d, want 1", len(calls))
			}
			got := calls[0]
			if got.fd != 37 || got.request != test.request || got.argument != uintptr(test.value) {
				t.Fatalf("ioctl call = %#v, want fd 37, request %#x, direct argument %#x", got, test.request, test.value)
			}
		})
	}
}

func TestStructIoctlsPassPointers(t *testing.T) {
	var got struct {
		fd       int
		request  uintptr
		argument uintptr
	}
	originalInvokeIoctl := invokeIoctl
	t.Cleanup(func() { invokeIoctl = originalInvokeIoctl })
	invokeIoctl = func(fd int, request uintptr, argument uintptr) error {
		got = struct {
			fd       int
			request  uintptr
			argument uintptr
		}{fd: fd, request: request, argument: argument}
		return nil
	}

	var setup uinputSetup
	if err := ioctl(41, uiDevSetup, unsafe.Pointer(&setup)); err != nil {
		t.Fatal(err)
	}
	if got.fd != 41 || got.request != uiDevSetup || got.argument != uintptr(unsafe.Pointer(&setup)) {
		t.Fatalf("ioctl call = %#v, want fd 41, request %#x, pointer %#x", got, uiDevSetup, uintptr(unsafe.Pointer(&setup)))
	}
}

func fakeUinputFile(fd uintptr) *os.File {
	return os.NewFile(fd, "uinput-test")
}

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
