//go:build linux

// Package hidraw provides the small amount of Linux HIDRAW plumbing needed by
// the daemon. It deliberately does not interpret HID reports.
package hidraw

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

const maxDescriptorSize = 4096

// pollTimeout bounds how long an I/O operation can remain asleep after Close
// races with poll. HIDRAW normally wakes poll when a report arrives; the
// timeout is only the fallback for Linux file-descriptor close semantics.
const pollTimeout = 100 // milliseconds

// Device is an opened HIDRAW character device.
//
// Access to the file handle is synchronized, and Close is allowed to run
// concurrently with ReadReport so a blocked read can be interrupted. A
// ReadReport reads one kernel HIDRAW report into the supplied buffer; callers
// should size it according to the device's report descriptor.
type Device struct {
	mu        sync.Mutex
	readMu    sync.Mutex
	writeMu   sync.Mutex
	file      *os.File
	writeFile *os.File
	path      string
	closed    bool
}

// Open opens path for bidirectional HIDRAW access. The read descriptor is
// nonblocking so ReadReport can use poll and observe Close; the separate write
// descriptor remains blocking because some HIDRAW devices reject nonblocking
// output writes.
func Open(path string) (*Device, error) {
	if path == "" {
		return nil, errors.New("hidraw: empty device path")
	}

	file, err := os.OpenFile(path, os.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("hidraw: open %q: %w", path, err)
	}
	writeFile, err := os.OpenFile(path, os.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("hidraw: open write %q: %w", path, err)
	}

	return &Device{file: file, writeFile: writeFile, path: path}, nil
}

// Path returns the path used to open the device.
func (d *Device) Path() string {
	if d == nil {
		return ""
	}
	return d.path
}

// FD returns the device file descriptor, or -1 after Close.
func (d *Device) FD() int {
	if d == nil {
		return -1
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return -1
	}
	return int(d.file.Fd())
}

// Close closes the device. It is safe to call more than once.
func (d *Device) Close() error {
	if d == nil {
		return nil
	}

	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	file := d.file
	writeFile := d.writeFile
	path := d.path
	d.mu.Unlock()

	var result error
	if file != nil {
		if err := file.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("hidraw: close read %q: %w", path, err))
		}
	}
	if writeFile != nil && writeFile != file {
		if err := writeFile.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("hidraw: close write %q: %w", path, err))
		}
	}
	return result
}

// ReadReport reads one report into dst. A short read is returned as-is because
// HIDRAW reports are delivered as individual reads and the kernel may return
// a report shorter than the caller's buffer.
func (d *Device) ReadReport(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, errors.New("hidraw: empty read buffer")
	}

	d.readMu.Lock()
	defer d.readMu.Unlock()

	fd, path, err := d.openFD()
	if err != nil {
		return 0, err
	}

	for {
		if d.isClosed() {
			return 0, ioError("read", path, os.ErrClosed)
		}

		revents, err := waitForEvent(fd, unix.POLLIN|unix.POLLPRI, d.isClosed)
		if err != nil {
			if d.isClosed() {
				err = os.ErrClosed
			}
			return 0, ioError("read", path, err)
		}
		if revents == 0 {
			continue
		}
		if revents&unix.POLLNVAL != 0 {
			if d.isClosed() {
				return 0, ioError("read", path, os.ErrClosed)
			}
			return 0, ioError("read", path, syscall.EBADF)
		}

		n, err := unix.Read(fd, dst)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if isWouldBlock(err) {
			continue
		}
		if d.isClosed() {
			return 0, ioError("read", path, os.ErrClosed)
		}
		if err != nil {
			return n, ioError("read", path, err)
		}
		if n == 0 {
			return 0, ioError("read", path, io.EOF)
		}
		return n, nil
	}
}

// WriteReport writes the complete report. EINTR is retried, short writes are
// completed, and terminal errors such as EPIPE are returned to the caller.
func (d *Device) WriteReport(report []byte) error {
	if len(report) == 0 {
		return errors.New("hidraw: empty report")
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	file, path, err := d.openWriteFile()
	if err != nil {
		return err
	}

	if _, err := writeAll(report, file.Write); err != nil {
		if d.isClosed() {
			err = os.ErrClosed
		}
		return ioError("write", path, err)
	}
	return nil
}

func (d *Device) openFD() (int, string, error) {
	if d == nil {
		return -1, "", os.ErrClosed
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpenLocked(); err != nil {
		return -1, "", err
	}
	return int(d.file.Fd()), d.path, nil
}

func (d *Device) openWriteFile() (*os.File, string, error) {
	if d == nil {
		return nil, "", os.ErrClosed
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpenLocked(); err != nil {
		return nil, "", err
	}
	if d.writeFile == nil {
		return nil, "", os.ErrClosed
	}
	return d.writeFile, d.path, nil
}

func (d *Device) checkOpenLocked() error {
	if d == nil || d.closed || d.file == nil {
		return os.ErrClosed
	}
	return nil
}

func (d *Device) isClosed() bool {
	if d == nil {
		return true
	}
	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	return closed
}

func waitForEvent(fd int, events int16, closed func() bool) (int16, error) {
	for {
		if closed != nil && closed() {
			return 0, os.ErrClosed
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: events}}
		_, err := unix.Poll(fds, pollTimeout)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if closed != nil && closed() {
			return 0, os.ErrClosed
		}
		return fds[0].Revents, nil
	}
}

func isWouldBlock(err error) bool {
	return errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK)
}

func ioError(operation, path string, err error) error {
	return fmt.Errorf("hidraw: %s %q: %w", operation, path, err)
}

// writeAll is kept independent of Device so short-write and syscall error
// handling can be tested without a HID device.
func writeAll(data []byte, write func([]byte) (int, error)) (int, error) {
	written := 0
	for written < len(data) {
		n, err := write(data[written:])
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return written, err
		}
		if n < 0 || n > len(data)-written {
			return written, fmt.Errorf("hidraw: invalid write count %d", n)
		}
		written += n
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

// RawInfo is the device identification returned by HIDIOCGRRAWINFO.
type RawInfo struct {
	BusType   uint32
	VendorID  uint16
	ProductID uint16
}

// GetRawInfo retrieves the kernel's raw HID bus, vendor, and product values.
func (d *Device) GetRawInfo() (RawInfo, error) {
	var raw struct {
		busType   uint32
		vendorID  uint16
		productID uint16
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpenLocked(); err != nil {
		return RawInfo{}, err
	}
	if err := ioctl(int(d.file.Fd()), hidIOCGRawInfo, unsafe.Pointer(&raw)); err != nil {
		return RawInfo{}, fmt.Errorf("hidraw: get raw info %q: %w", d.path, err)
	}

	return RawInfo{
		BusType:   raw.busType,
		VendorID:  raw.vendorID,
		ProductID: raw.productID,
	}, nil
}

// GetReportDescriptor retrieves the raw HID report descriptor.
func (d *Device) GetReportDescriptor() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpenLocked(); err != nil {
		return nil, err
	}

	var size int32
	if err := ioctl(int(d.file.Fd()), hidIOCGRDescSize, unsafe.Pointer(&size)); err != nil {
		return nil, fmt.Errorf("hidraw: get descriptor size %q: %w", d.path, err)
	}
	if size < 0 || size > maxDescriptorSize {
		return nil, fmt.Errorf("hidraw: invalid descriptor size %d", size)
	}

	var descriptor struct {
		size  uint32
		value [maxDescriptorSize]byte
	}
	descriptor.size = uint32(size)
	if err := ioctl(int(d.file.Fd()), hidIOCGRDesc, unsafe.Pointer(&descriptor)); err != nil {
		return nil, fmt.Errorf("hidraw: get descriptor %q: %w", d.path, err)
	}
	if descriptor.size > maxDescriptorSize {
		return nil, fmt.Errorf("hidraw: kernel returned invalid descriptor size %d", descriptor.size)
	}

	result := make([]byte, descriptor.size)
	copy(result, descriptor.value[:descriptor.size])
	return result, nil
}

const (
	iocRead     = 2
	iocTypeBits = 8
	iocSizeBits = 14
	iocNRBits   = 8

	iocNRShift   = 0
	iocTypeShift = iocNRShift + iocNRBits
	iocSizeShift = iocTypeShift + iocTypeBits
	iocDirShift  = iocSizeShift + iocSizeBits
)

func ioc(direction, typ, number, size uintptr) uintptr {
	return (direction << iocDirShift) |
		(typ << iocTypeShift) |
		(number << iocNRShift) |
		(size << iocSizeShift)
}

var (
	hidIOCGRDescSize = ioc(iocRead, uintptr('H'), 0x01, 4)
	hidIOCGRDesc     = ioc(iocRead, uintptr('H'), 0x02, 4+maxDescriptorSize)
	hidIOCGRawInfo   = ioc(iocRead, uintptr('H'), 0x03, 8)
)

func ioctl(fd int, request uintptr, argument unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, uintptr(argument))
	if errno != 0 {
		return errno
	}
	return nil
}
