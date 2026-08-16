//go:build linux

package hidraw

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWriteAllRetriesInterruptedAndShortWrites(t *testing.T) {
	data := []byte("abcdef")
	var calls int
	got, err := writeAll(data, func(part []byte) (int, error) {
		calls++
		switch calls {
		case 1:
			return 0, syscall.EINTR
		case 2:
			return 2, nil
		case 3:
			return len(part), nil
		default:
			t.Fatalf("unexpected write call %d", calls)
			return 0, nil
		}
	})
	if err != nil {
		t.Fatalf("writeAll returned error: %v", err)
	}
	if got != len(data) || calls != 3 {
		t.Fatalf("writeAll wrote %d bytes in %d calls, want %d bytes in 3 calls", got, calls, len(data))
	}
}

func TestWriteAllReturnsEPIPE(t *testing.T) {
	got, err := writeAll([]byte("abc"), func([]byte) (int, error) {
		return 0, syscall.EPIPE
	})
	if !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("writeAll error = %v, want EPIPE", err)
	}
	if got != 0 {
		t.Fatalf("writeAll wrote %d bytes, want 0", got)
	}
}

func TestWriteAllRejectsInvalidWriterResults(t *testing.T) {
	for name, write := range map[string]func([]byte) (int, error){
		"negative":  func([]byte) (int, error) { return -1, nil },
		"too large": func(part []byte) (int, error) { return len(part) + 1, nil },
		"zero":      func([]byte) (int, error) { return 0, nil },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := writeAll([]byte("abc"), write); err == nil {
				t.Fatal("writeAll returned nil error")
			}
		})
	}
}

func TestOpenPathAndCloseWithoutHardware(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "hidraw-test-")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	device, err := Open(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if got := device.Path(); got != file.Name() {
		t.Fatalf("Path() = %q, want %q", got, file.Name())
	}
	if device.FD() < 0 {
		t.Fatal("FD() returned a closed descriptor")
	}
	if err := device.Close(); err != nil {
		t.Fatal(err)
	}
	if err := device.Close(); err != nil {
		t.Fatalf("second Close() returned error: %v", err)
	}
	if got := device.FD(); got != -1 {
		t.Fatalf("FD() after close = %d, want -1", got)
	}
}

func TestReadAndWriteReportWithoutHardware(t *testing.T) {
	path := t.TempDir() + "/report"
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	device, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()

	want := []byte{0x10, 0x01, 0x02, 0x03}
	if err := device.WriteReport(want); err != nil {
		t.Fatal(err)
	}
	if err := device.Close(); err != nil {
		t.Fatal(err)
	}

	device, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	got := make([]byte, len(want))
	n, err := device.ReadReport(got)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(want) || string(got) != string(want) {
		t.Fatalf("ReadReport() = %d, %x; want %d, %x", n, got, len(want), want)
	}
}

func TestReadReportPollsForData(t *testing.T) {
	device, writer := newPipeDevice(t)
	defer writer.Close()
	defer device.Close()

	want := []byte{0x10, 0x20, 0x30}
	go func() {
		time.Sleep(10 * time.Millisecond)
		_, _ = writer.Write(want)
	}()

	got := make([]byte, len(want))
	n, err := device.ReadReport(got)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(want) || string(got) != string(want) {
		t.Fatalf("ReadReport() = %d, %x; want %d, %x", n, got, len(want), want)
	}
}

func TestReadReportReturnsPromptlyAfterClose(t *testing.T) {
	device, writer := newPipeDevice(t)
	defer writer.Close()

	result := make(chan error, 1)
	go func() {
		_, err := device.ReadReport(make([]byte, 8))
		result <- err
	}()
	time.Sleep(10 * time.Millisecond)

	started := time.Now()
	if err := device.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, os.ErrClosed) {
			t.Fatalf("ReadReport error = %v, want os.ErrClosed", err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("ReadReport took %s to return after Close", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadReport did not return after Close")
	}
}

func newPipeDevice(t *testing.T) (*Device, *os.File) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.SetNonblock(int(reader.Fd()), true); err != nil {
		reader.Close()
		writer.Close()
		t.Fatal(err)
	}
	return &Device{file: reader, path: "pipe"}, writer
}
