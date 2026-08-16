//go:build !linux

package uinput

import (
	"errors"
	"fmt"
)

// Options mirrors the Linux constructor so callers can keep platform-neutral
// lifecycle code. A real virtual input device is currently Linux-only.
type Options struct {
	Path      string
	Name      string
	VendorID  uint16
	ProductID uint16
	Version   uint16
	Keys      []uint16
	Axes      []uint16
}

type Device struct{}

func Open(options ...Options) (*Device, error) {
	if len(options) > 1 {
		return nil, errors.New("uinput: multiple options values")
	}
	return nil, fmt.Errorf("uinput: virtual input devices are unsupported on this platform")
}

func New(options ...Options) (*Device, error) { return Open(options...) }

func OpenPath(path string, options ...Options) (*Device, error) {
	return Open(options...)
}

func (d *Device) Emit(Event) error {
	return errors.New("uinput: virtual input devices are unsupported on this platform")
}
func (d *Device) Sync() error {
	return errors.New("uinput: virtual input devices are unsupported on this platform")
}
func (d *Device) Close() error                    { return nil }
func (d *Device) Supports(EventType, uint16) bool { return false }
func (d *Device) FD() int                         { return -1 }
