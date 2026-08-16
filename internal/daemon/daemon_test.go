package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atremb/logitechd/internal/config"
	"github.com/atremb/logitechd/internal/hidpp"
	"github.com/atremb/logitechd/internal/mxmaster"
	"github.com/atremb/logitechd/internal/receiver"
	"github.com/atremb/logitechd/internal/uinput"
)

type quietLogger struct{}

func (quietLogger) Printf(string, ...any) {}

type fakeSession struct {
	done chan struct{}

	mu       sync.Mutex
	terminal error
	closed   bool
}

func newFakeSession() *fakeSession { return &fakeSession{done: make(chan struct{})} }

func (s *fakeSession) Children() []*receiver.ChildDevice { return nil }
func (s *fakeSession) Receiver() *receiver.Receiver      { return nil }
func (s *fakeSession) Done() <-chan struct{}             { return s.done }
func (s *fakeSession) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal
}
func (s *fakeSession) Close() error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	s.mu.Unlock()
	return nil
}
func (s *fakeSession) lose(err error) {
	s.mu.Lock()
	s.terminal = err
	s.mu.Unlock()
	_ = s.Close()
}

type fakeOutput struct {
	mu     sync.Mutex
	closed int
}

func (*fakeOutput) Emit(uinput.Event) error { return nil }
func (*fakeOutput) Sync() error             { return nil }
func (o *fakeOutput) Close() error {
	o.mu.Lock()
	o.closed++
	o.mu.Unlock()
	return nil
}

type fakeAction struct {
	mu     sync.Mutex
	reset  int
	stop   int
	output mxmaster.Output
	errors chan error
}

func (a *fakeAction) Errors() <-chan error { return a.errors }
func (a *fakeAction) Reset() error {
	a.mu.Lock()
	a.reset++
	a.mu.Unlock()
	return nil
}
func (a *fakeAction) Stop() error {
	a.mu.Lock()
	a.stop++
	output := a.output
	a.mu.Unlock()
	if output != nil {
		return output.Close()
	}
	return nil
}

type fakeConfigurator struct {
	mu       sync.Mutex
	apply    int
	closed   int
	action   *fakeAction
	applyErr error
}

func (c *fakeConfigurator) Apply(context.Context) error {
	c.mu.Lock()
	c.apply++
	err := c.applyErr
	c.mu.Unlock()
	return err
}
func (c *fakeConfigurator) StartActions(_ context.Context, output mxmaster.Output) (Action, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.action == nil {
		c.action = &fakeAction{errors: make(chan error, 1)}
	}
	c.action.mu.Lock()
	c.action.output = output
	c.action.mu.Unlock()
	return c.action, nil
}
func (c *fakeConfigurator) Close() error {
	c.mu.Lock()
	c.closed++
	c.mu.Unlock()
	return nil
}

func (c *fakeConfigurator) actionStopped() bool {
	c.mu.Lock()
	action := c.action
	c.mu.Unlock()
	if action == nil {
		return false
	}
	action.mu.Lock()
	defer action.mu.Unlock()
	return action.stop == 1
}

type sessionOffer struct {
	session *fakeSession
	onEvent func(receiver.ChildEvent)
}

func validSettings() config.Config {
	return config.Config{
		Receiver: config.ReceiverConfig{Type: config.ReceiverBolt},
		Device:   config.DeviceConfig{Name: "MX Master 3S", Index: 2},
	}
}

func newTestDaemon(t *testing.T, settings config.Config, offers chan<- sessionOffer, configure func() *fakeConfigurator) (*Daemon, context.CancelFunc, <-chan error, *fakeOutput) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	factoryErrors := make(chan error, 1)
	output := &fakeOutput{}
	run, err := New(settings, Options{
		Receiver: receiver.Options{Kind: receiver.KindBolt},
		SessionFactory: func(_ context.Context, options receiver.LifecycleOptions) (Session, error) {
			session := newFakeSession()
			offers <- sessionOffer{session: session, onEvent: options.Callbacks.Event}
			return session, nil
		},
		Configurator: func() ConfiguratorFactory {
			return func(context.Context, Session, config.Config) (Configurator, error) {
				return configure(), nil
			}
		}(),
		OutputFactory: func() (mxmaster.Output, error) { return output, nil },
		RetryInterval: time.Millisecond,
		Logger:        quietLogger{},
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	go func() { factoryErrors <- run.Run(ctx) }()
	return run, cancel, factoryErrors, output
}

func TestValidateConfigDoesNotOpenHardware(t *testing.T) {
	settings := validSettings()
	settings.Buttons = map[config.CID]config.ActionSpec{
		0x53: {Action: "key", Value: "KEY_A"},
	}
	if err := ValidateConfig(settings); err != nil {
		t.Fatal(err)
	}
	settings.Buttons[0x53] = config.ActionSpec{Action: "key", Value: "not-a-real-key"}
	if err := ValidateConfig(settings); err == nil || !strings.Contains(err.Error(), "validate actions") {
		t.Fatalf("invalid action error = %v", err)
	}
}

func TestDaemonSelectsTargetAndCleansUpOnShutdown(t *testing.T) {
	offers := make(chan sessionOffer, 1)
	created := make(chan *fakeConfigurator, 1)
	_, cancel, results, output := newTestDaemon(t, validSettings(), offers, func() *fakeConfigurator {
		configurator := &fakeConfigurator{}
		created <- configurator
		return configurator
	})
	offer := <-offers
	offer.onEvent(receiver.ChildEvent{Type: receiver.ChildReady, Metadata: receiver.ChildMetadata{WirelessIndex: 1, Name: "Other"}})
	select {
	case <-created:
		t.Fatal("non-target child was configured")
	case <-time.After(20 * time.Millisecond):
	}
	offer.onEvent(receiver.ChildEvent{Type: receiver.ChildReady, Metadata: receiver.ChildMetadata{WirelessIndex: 2, Name: "MX Master 3S"}})
	var configurator *fakeConfigurator
	select {
	case configurator = <-created:
	case <-time.After(time.Second):
		t.Fatal("target was not configured")
	}
	cancel()
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if configurator.action == nil || configurator.action.reset != 1 || configurator.action.stop != 1 {
		t.Fatalf("action cleanup = %+v", configurator.action)
	}
	if configurator.closed != 1 || output.closed != 1 {
		t.Fatalf("configurator/output cleanup = %d/%d", configurator.closed, output.closed)
	}
}

func TestDaemonHandlesSleepWakeAndRemoval(t *testing.T) {
	offers := make(chan sessionOffer, 1)
	created := make(chan *fakeConfigurator, 2)
	_, cancel, results, _ := newTestDaemon(t, validSettings(), offers, func() *fakeConfigurator {
		configurator := &fakeConfigurator{}
		created <- configurator
		return configurator
	})
	offer := <-offers
	target := receiver.ChildMetadata{WirelessIndex: 2, Name: "MX Master 3S"}
	offer.onEvent(receiver.ChildEvent{Type: receiver.ChildReady, Metadata: target})
	first := <-created
	offer.onEvent(receiver.ChildEvent{Type: receiver.ChildSleeping, Metadata: target})
	waitFor(t, first.actionStopped)
	offer.onEvent(receiver.ChildEvent{Type: receiver.ChildReady, Metadata: target})
	second := <-created
	offer.onEvent(receiver.ChildEvent{Type: receiver.ChildRemoved, Metadata: target})
	waitFor(t, second.actionStopped)
	cancel()
	if err := <-results; err != nil {
		t.Fatal(err)
	}
}

func TestDaemonRetriesTransientTargetConfiguration(t *testing.T) {
	offers := make(chan sessionOffer, 1)
	created := make(chan *fakeConfigurator, 2)
	attempt := 0
	_, cancel, results, _ := newTestDaemon(t, validSettings(), offers, func() *fakeConfigurator {
		attempt++
		configured := &fakeConfigurator{}
		if attempt == 1 {
			configured.applyErr = hidpp.ErrTimeout
		}
		created <- configured
		return configured
	})
	offer := <-offers
	offer.onEvent(receiver.ChildEvent{Type: receiver.ChildReady, Metadata: receiver.ChildMetadata{WirelessIndex: 2, Name: "MX Master 3S"}})
	<-created
	select {
	case <-created:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("transient target configuration was not retried")
	}
	cancel()
	if err := <-results; err != nil {
		t.Fatal(err)
	}
}

func TestDaemonReconnectsAfterReceiverLoss(t *testing.T) {
	offers := make(chan sessionOffer, 2)
	created := make(chan *fakeConfigurator, 2)
	_, cancel, results, _ := newTestDaemon(t, validSettings(), offers, func() *fakeConfigurator {
		configurator := &fakeConfigurator{}
		created <- configurator
		return configurator
	})
	firstOffer := <-offers
	target := receiver.ChildMetadata{WirelessIndex: 2, Name: "MX Master 3S"}
	firstOffer.onEvent(receiver.ChildEvent{Type: receiver.ChildReady, Metadata: target})
	firstConfig := <-created
	firstOffer.session.lose(errors.New("receiver removed"))
	secondOffer := <-offers
	secondOffer.onEvent(receiver.ChildEvent{Type: receiver.ChildReady, Metadata: target})
	secondConfig := <-created
	cancel()
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if firstConfig.action == nil || firstConfig.action.stop != 1 || secondConfig.action == nil || secondConfig.action.stop != 1 {
		t.Fatalf("reconnect cleanup = first=%+v second=%+v", firstConfig.action, secondConfig.action)
	}
}

func TestDaemonPropagatesReceiverErrors(t *testing.T) {
	want := errors.New("permission denied")
	settings := validSettings()
	run, err := New(settings, Options{
		SessionFactory: func(context.Context, receiver.LifecycleOptions) (Session, error) {
			return nil, want
		},
		RetryInterval: time.Millisecond,
		Logger:        quietLogger{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Run() error = %v, want %v", err, want)
	}
}

func TestDaemonTreatsCancellationDuringOpenAsCleanShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run, err := New(validSettings(), Options{
		SessionFactory: func(context.Context, receiver.LifecycleOptions) (Session, error) {
			cancel()
			return nil, context.Canceled
		},
		Logger: quietLogger{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v, want clean shutdown", err)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
