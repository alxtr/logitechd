//go:build linux

package uinput

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const defaultPath = "/dev/uinput"

// Options controls creation of a virtual relative-pointer device. The kernel
// only permits capabilities to be registered before UI_DEV_CREATE, so Keys
// and Axes are copied and registered during Open.
type Options struct {
	Path      string
	Name      string
	VendorID  uint16
	ProductID uint16
	Version   uint16
	Keys      []uint16
	Axes      []uint16
}

// Device is a created Linux uinput device. Writes and lifecycle operations are
// serialized, and Close can safely race with Emit or Sync.
type Device struct {
	mu        sync.Mutex
	file      *os.File
	path      string
	created   bool
	closed    bool
	closeOnce sync.Once
	closeErr  error
	axes      map[uint16]bool
	keys      map[uint16]bool
}

// Open creates a device using the default path and capabilities. The optional
// form is useful for tests and for systems that expose uinput elsewhere.
func Open(options ...Options) (*Device, error) {
	if len(options) > 1 {
		return nil, errors.New("uinput: multiple options values")
	}
	var optionsValue Options
	if len(options) == 1 {
		optionsValue = options[0]
	}
	path := optionsValue.Path
	if path == "" {
		path = defaultPath
	}
	name := optionsValue.Name
	if name == "" {
		name = "logitechd virtual pointer"
	}
	if len(name) >= 80 {
		return nil, fmt.Errorf("uinput: device name is too long (%d bytes, maximum is 79)", len(name))
	}

	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("uinput: open %q: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	device := &Device{
		file: file,
		path: path,
		axes: make(map[uint16]bool),
		keys: make(map[uint16]bool),
	}
	closeOnError := func(operation string, operationErr error) (*Device, error) {
		_ = file.Close()
		return nil, fmt.Errorf("uinput: %s %q: %w", operation, path, operationErr)
	}

	if err := device.setEventBit(EV_KEY); err != nil {
		return closeOnError("enable key events", err)
	}
	if err := device.setEventBit(EV_REL); err != nil {
		return closeOnError("enable relative events", err)
	}
	keys := append(append([]uint16(nil), DefaultKeys...), optionsValue.Keys...)
	for _, code := range uniqueCodes(keys) {
		if err := device.setKeyBit(code); err != nil {
			return closeOnError(fmt.Sprintf("register key 0x%x", code), err)
		}
		device.keys[code] = true
	}
	axes := optionsValue.Axes
	if len(axes) == 0 {
		axes = []uint16{REL_X, REL_Y, REL_WHEEL, REL_HWHEEL, REL_WHEEL_HI_RES, REL_HWHEEL_HI_RES}
	}
	for _, code := range uniqueCodes(axes) {
		if err := device.setRelBit(code); err != nil {
			if isOptionalHiResAxis(code) && isOptionalAxisError(err) {
				continue
			}
			return closeOnError(fmt.Sprintf("register relative axis 0x%x", code), err)
		}
		device.axes[code] = true
	}

	var setup uinputSetup
	setup.ID = inputID{BusType: unix.BUS_USB, Vendor: optionsValue.VendorID, Product: optionsValue.ProductID, Version: optionsValue.Version}
	copy(setup.Name[:], name)
	if err := ioctl(fd, uiDevSetup, unsafe.Pointer(&setup)); err != nil {
		if !errors.Is(err, syscall.EINVAL) {
			return closeOnError("configure device", err)
		}
		// UI_DEV_SETUP was added after the original uinput ABI. Keep a
		// standards-based fallback for kernels that still expose only the
		// uinput_user_dev write interface.
		legacy := uinputUserDev{ID: setup.ID, FFEffectsMax: setup.FFEffectsMax}
		copy(legacy.Name[:], setup.Name[:])
		data := unsafe.Slice((*byte)(unsafe.Pointer(&legacy)), unsafe.Sizeof(legacy))
		if _, writeErr := writeAll(data, file.Write); writeErr != nil {
			return closeOnError("configure device", errors.Join(err, writeErr))
		}
	}
	if err := ioctl(fd, uiDevCreate, nil); err != nil {
		return closeOnError("create device", err)
	}
	device.created = true
	return device, nil
}

// New is an explicit constructor alias for Open.
func New(options ...Options) (*Device, error) { return Open(options...) }

// OpenPath is convenient when only the uinput node needs to be overridden.
func OpenPath(path string, options ...Options) (*Device, error) {
	if len(options) > 1 {
		return nil, errors.New("uinput: multiple options values")
	}
	var value Options
	if len(options) == 1 {
		value = options[0]
	}
	value.Path = path
	return Open(value)
}

func (d *Device) Emit(event Event) error {
	if d == nil {
		return errors.New("uinput: nil device")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpenLocked(); err != nil {
		return err
	}
	if event.Type == EV_KEY {
		if !d.keys[event.Code] {
			return fmt.Errorf("uinput: key 0x%x was not registered", event.Code)
		}
	} else if event.Type == EV_REL {
		if !d.axes[event.Code] {
			return fmt.Errorf("uinput: relative axis 0x%x was not registered", event.Code)
		}
	} else if event.Type != EV_SYN {
		return fmt.Errorf("uinput: unsupported event type 0x%x", event.Type)
	}
	return d.writeEventLocked(event)
}

func (d *Device) Sync() error {
	if d == nil {
		return errors.New("uinput: nil device")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpenLocked(); err != nil {
		return err
	}
	return d.writeEventLocked(Event{Type: EV_SYN, Code: SYN_REPORT})
}

func (d *Device) Supports(kind EventType, code uint16) bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return false
	}
	switch kind {
	case EV_KEY:
		return d.keys[code]
	case EV_REL:
		return d.axes[code]
	default:
		return kind == EV_SYN && code == SYN_REPORT
	}
}

// Close destroys the kernel device before closing the uinput node. It is
// idempotent and returns all teardown errors, including a failed destroy.
func (d *Device) Close() error {
	if d == nil {
		return nil
	}
	d.closeOnce.Do(func() {
		d.mu.Lock()
		if d.closed {
			d.mu.Unlock()
			return
		}
		d.closed = true
		if d.created {
			if err := ioctl(int(d.file.Fd()), uiDevDestroy, nil); err != nil {
				d.closeErr = fmt.Errorf("uinput: destroy device %q: %w", d.path, err)
			}
			d.created = false
		}
		if err := d.file.Close(); err != nil {
			d.closeErr = errors.Join(d.closeErr, fmt.Errorf("uinput: close %q: %w", d.path, err))
		}
		d.mu.Unlock()
	})
	return d.closeErr
}

func (d *Device) FD() int {
	if d == nil {
		return -1
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.file == nil {
		return -1
	}
	return int(d.file.Fd())
}

func (d *Device) checkOpenLocked() error {
	if d.closed || d.file == nil || !d.created {
		return os.ErrClosed
	}
	return nil
}

func (d *Device) writeEventLocked(event Event) error {
	wire := inputEvent{Type: uint16(event.Type), Code: event.Code, Value: event.Value}
	data := unsafe.Slice((*byte)(unsafe.Pointer(&wire)), unsafe.Sizeof(wire))
	if _, err := writeAll(data, d.file.Write); err != nil {
		return fmt.Errorf("uinput: write event type 0x%x code 0x%x: %w", event.Type, event.Code, err)
	}
	return nil
}

func writeAll(data []byte, write func([]byte) (int, error)) (int, error) {
	written := 0
	for written < len(data) {
		n, err := write(data[written:])
		if n < 0 || n > len(data)-written {
			return written, fmt.Errorf("uinput: invalid write count %d", n)
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		written += n
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

type inputID struct {
	BusType uint16
	Vendor  uint16
	Product uint16
	Version uint16
}

type uinputSetup struct {
	ID           inputID
	Name         [80]byte
	FFEffectsMax uint32
}

type uinputUserDev struct {
	Name         [80]byte
	ID           inputID
	FFEffectsMax uint32
	AbsMax       [64]int32
	AbsMin       [64]int32
	AbsFuzz      [64]int32
	AbsFlat      [64]int32
}

// input_event uses the native Linux timeval ABI. Zero timestamps are accepted
// by uinput and avoid a needless clock syscall for every action report.
type inputEvent struct {
	Time  unix.Timeval
	Type  uint16
	Code  uint16
	Value int32
	Pad   int32
}

const (
	iocNRBits    = 8
	iocTypeBits  = 8
	iocSizeBits  = 14
	iocNRShift   = 0
	iocTypeShift = iocNRShift + iocNRBits
	iocSizeShift = iocTypeShift + iocTypeBits
	iocDirShift  = iocSizeShift + iocSizeBits
	iocWrite     = 1
)

func ioc(direction uintptr, typ byte, number byte, size uintptr) uintptr {
	return (direction << iocDirShift) |
		(uintptr(typ) << iocTypeShift) |
		(uintptr(number) << iocNRShift) |
		(size << iocSizeShift)
}

func iow(typ byte, number byte, size uintptr) uintptr { return ioc(iocWrite, typ, number, size) }
func ioNone(typ byte, number byte) uintptr            { return ioc(0, typ, number, 0) }

var (
	uiSetEvBit   = iow('U', 100, 4)
	uiSetKeyBit  = iow('U', 101, 4)
	uiSetRelBit  = iow('U', 102, 4)
	uiDevCreate  = ioNone('U', 1)
	uiDevDestroy = ioNone('U', 2)
	uiDevSetup   = iow('U', 3, unsafe.Sizeof(uinputSetup{}))
)

func (d *Device) setEventBit(kind EventType) error {
	return ioctlInt(int(d.file.Fd()), uiSetEvBit, int(kind))
}

func (d *Device) setKeyBit(code uint16) error {
	return ioctlInt(int(d.file.Fd()), uiSetKeyBit, int(code))
}

func (d *Device) setRelBit(code uint16) error {
	return ioctlInt(int(d.file.Fd()), uiSetRelBit, int(code))
}

func ioctlInt(fd int, request uintptr, value int) error {
	return ioctl(fd, request, unsafe.Pointer(&value))
}

func ioctl(fd int, request uintptr, argument unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, uintptr(argument))
	if errno != 0 {
		return errno
	}
	return nil
}

func isOptionalHiResAxis(code uint16) bool {
	return code == REL_WHEEL_HI_RES || code == REL_HWHEEL_HI_RES
}

func isOptionalAxisError(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.ENOSYS)
}
