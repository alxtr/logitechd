package power

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

type fakeSignalConnection struct {
	ctx        context.Context
	cancel     context.CancelFunc
	registered chan struct{}
	once       sync.Once

	mu           sync.Mutex
	signals      chan<- *dbus.Signal
	matchOptions int
}

func newFakeSignalConnection() *fakeSignalConnection {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeSignalConnection{
		ctx:        ctx,
		cancel:     cancel,
		registered: make(chan struct{}),
	}
}

func (c *fakeSignalConnection) AddMatchSignalContext(_ context.Context, options ...dbus.MatchOption) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.signals == nil {
		return errors.New("signal channel was not registered before match")
	}
	c.matchOptions = len(options)
	c.once.Do(func() { close(c.registered) })
	return nil
}

func (c *fakeSignalConnection) Signal(signals chan<- *dbus.Signal) {
	c.mu.Lock()
	c.signals = signals
	c.mu.Unlock()
}

func (c *fakeSignalConnection) RemoveSignal(signals chan<- *dbus.Signal) {
	c.mu.Lock()
	if c.signals == signals {
		c.signals = nil
	}
	c.mu.Unlock()
}

func (c *fakeSignalConnection) Context() context.Context { return c.ctx }

func (c *fakeSignalConnection) Close() error {
	c.cancel()
	return nil
}

func (c *fakeSignalConnection) emit(signal *dbus.Signal) {
	c.mu.Lock()
	signals := c.signals
	c.mu.Unlock()
	if signals != nil {
		signals <- signal
	}
}

func (c *fakeSignalConnection) optionsCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.matchOptions
}

func TestIsResumeSignal(t *testing.T) {
	tests := []struct {
		name   string
		signal *dbus.Signal
		want   bool
	}{
		{name: "nil"},
		{
			name: "resume",
			signal: &dbus.Signal{
				Path: loginManagerPath,
				Name: prepareForSleep,
				Body: []any{false},
			},
			want: true,
		},
		{
			name: "suspend",
			signal: &dbus.Signal{
				Path: loginManagerPath,
				Name: prepareForSleep,
				Body: []any{true},
			},
		},
		{
			name: "wrong member",
			signal: &dbus.Signal{
				Path: loginManagerPath,
				Name: "org.freedesktop.login1.Manager.Other",
				Body: []any{false},
			},
		},
		{
			name: "malformed body",
			signal: &dbus.Signal{
				Path: loginManagerPath,
				Name: prepareForSleep,
				Body: []any{"false"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isResumeSignal(test.signal); got != test.want {
				t.Fatalf("isResumeSignal() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestWatchResumesFiltersSignalsAndReconnects(t *testing.T) {
	originalConnect := connectSystemBus
	originalInterval := reconnectInterval
	t.Cleanup(func() {
		connectSystemBus = originalConnect
		reconnectInterval = originalInterval
	})

	first := newFakeSignalConnection()
	second := newFakeSignalConnection()
	connections := make(chan signalConnection, 2)
	connections <- first
	connections <- second
	connectSystemBus = func(ctx context.Context) (signalConnection, error) {
		select {
		case connection := <-connections:
			return connection, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	reconnectInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	resumes, err := WatchResumes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waitForRegistration(t, first)
	if got := first.optionsCount(); got != 4 {
		t.Fatalf("D-Bus match option count = %d, want sender, path, interface, and member", got)
	}

	first.emit(&dbus.Signal{Path: loginManagerPath, Name: prepareForSleep, Body: []any{true}})
	assertNoResume(t, resumes)
	first.emit(&dbus.Signal{Path: loginManagerPath, Name: prepareForSleep, Body: []any{false}})
	assertResume(t, resumes)

	first.cancel()
	waitForRegistration(t, second)
	assertResume(t, resumes)
	second.emit(&dbus.Signal{Path: loginManagerPath, Name: prepareForSleep, Body: []any{false}})
	assertResume(t, resumes)

	cancel()
	select {
	case _, ok := <-resumes:
		if ok {
			t.Fatal("unexpected resume while stopping monitor")
		}
	case <-time.After(time.Second):
		t.Fatal("resume channel did not close after cancellation")
	}
}

func TestWatchResumesRetriesInitialSubscriptionFailure(t *testing.T) {
	originalConnect := connectSystemBus
	originalInterval := reconnectInterval
	t.Cleanup(func() {
		connectSystemBus = originalConnect
		reconnectInterval = originalInterval
	})

	connection := newFakeSignalConnection()
	attempts := 0
	connectSystemBus = func(context.Context) (signalConnection, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("system bus temporarily unavailable")
		}
		return connection, nil
	}
	reconnectInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	resumes, err := WatchResumes(ctx)
	if err == nil {
		cancel()
		t.Fatal("initial subscription failure was not reported")
	}
	if resumes == nil {
		cancel()
		t.Fatal("initial subscription failure disabled resume monitoring")
	}
	waitForRegistration(t, connection)
	assertResume(t, resumes)

	cancel()
	select {
	case _, ok := <-resumes:
		if ok {
			t.Fatal("unexpected resume while stopping monitor")
		}
	case <-time.After(time.Second):
		t.Fatal("resume channel did not close after cancellation")
	}
}

func waitForRegistration(t *testing.T, connection *fakeSignalConnection) {
	t.Helper()
	select {
	case <-connection.registered:
	case <-time.After(time.Second):
		t.Fatal("D-Bus subscription was not registered")
	}
}

func assertResume(t *testing.T, resumes <-chan struct{}) {
	t.Helper()
	select {
	case <-resumes:
	case <-time.After(time.Second):
		t.Fatal("resume event was not forwarded")
	}
}

func assertNoResume(t *testing.T, resumes <-chan struct{}) {
	t.Helper()
	select {
	case <-resumes:
		t.Fatal("suspend event was forwarded as a resume")
	case <-time.After(20 * time.Millisecond):
	}
}
