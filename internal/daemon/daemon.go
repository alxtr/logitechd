// Package daemon owns the process-level lifecycle for logitechd. It connects
// the receiver session, MX Master feature configurator, and the action output
// without making any of those lower-level packages responsible for process
// signals or reconnect policy.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"syscall"
	"time"

	"github.com/atremb/logitechd/internal/config"
	"github.com/atremb/logitechd/internal/hidpp"
	"github.com/atremb/logitechd/internal/mxmaster"
	"github.com/atremb/logitechd/internal/receiver"
	"github.com/atremb/logitechd/internal/uinput"
)

const defaultRetryInterval = 2 * time.Second

// Logger is intentionally the small Printf surface used by the standard
// logger. It keeps orchestration tests independent of process logging.
type Logger interface {
	Printf(string, ...any)
}

// Session is the persistent receiver surface required by the daemon. The
// health methods are what allow a physical HIDRAW loss to trigger discovery
// again instead of ending the process.
type Session interface {
	Children() []*receiver.ChildDevice
	Receiver() *receiver.Receiver
	Done() <-chan struct{}
	Err() error
	Close() error
}

// SessionFactory is injectable so orchestration tests never need a HIDRAW
// device. The supplied lifecycle callbacks must be retained by real and fake
// factories alike.
type SessionFactory func(context.Context, receiver.LifecycleOptions) (Session, error)

// Configurator is the small subset of the Phase 5 configurator used by the
// process owner. ActionHandler is deliberately hidden behind Action so tests
// can exercise lifecycle transitions with a fake.
type Configurator interface {
	Apply(context.Context) error
	StartActions(context.Context, mxmaster.Output) (Action, error)
	Close() error
}

// Action is the lifecycle surface of the Phase 6 action handler.
type Action interface {
	Errors() <-chan error
	Reset() error
	Stop() error
}

// ConfiguratorFactory is injectable for hardware-free daemon tests. The
// production implementation delegates directly to mxmaster.Configurator.
type ConfiguratorFactory func(context.Context, Session, config.Config) (Configurator, error)

// OutputFactory creates the uinput sink only after the configured child is
// ready. This avoids opening /dev/uinput while the receiver or child is absent.
type OutputFactory func() (mxmaster.Output, error)

// Options controls daemon dependencies and reconnect timing.
type Options struct {
	Receiver       receiver.Options
	SessionFactory SessionFactory
	Configurator   ConfiguratorFactory
	OutputFactory  OutputFactory
	RetryInterval  time.Duration
	Logger         Logger
}

// Daemon is a single-run process lifecycle. Run blocks until the context is
// canceled, the receiver reconnects forever, or a non-recoverable operational
// error is encountered.
type Daemon struct {
	settings config.Config
	options  Options
	logger   Logger
}

// New validates settings without opening hardware and prepares a daemon.
func New(settings config.Config, options Options) (*Daemon, error) {
	if err := ValidateConfig(settings); err != nil {
		return nil, err
	}
	if options.SessionFactory == nil {
		options.SessionFactory = defaultSessionFactory
	}
	if options.Configurator == nil {
		options.Configurator = defaultConfiguratorFactory
	}
	if options.OutputFactory == nil {
		options.OutputFactory = defaultOutputFactory
	}
	if options.RetryInterval <= 0 {
		options.RetryInterval = defaultRetryInterval
	}
	if options.Logger == nil {
		options.Logger = log.Default()
	}
	return &Daemon{settings: settings, options: options, logger: options.Logger}, nil
}

// ValidateConfig performs all hardware-free checks needed by the daemon,
// including action key compilation. It never opens HIDRAW or uinput.
func ValidateConfig(settings config.Config) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	fake := uinput.NewFakeOutput()
	handler, err := mxmaster.NewActionHandler(settings, fake)
	if err != nil {
		return fmt.Errorf("daemon: validate actions: %w", err)
	}
	if err := handler.Stop(); err != nil {
		return fmt.Errorf("daemon: validate actions: %w", err)
	}
	return nil
}

// Run owns the reconnect loop and returns only for cancellation, a clean
// shutdown, or an error that should be visible to systemd and the operator.
func (d *Daemon) Run(ctx context.Context) error {
	if d == nil {
		return errors.New("daemon: nil daemon")
	}
	if ctx == nil {
		return errors.New("daemon: nil context")
	}

	events := make(chan lifecycleSignal, 256)
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		token := &connectionToken{}
		lifecycle := receiver.LifecycleOptions{
			Receiver:  d.options.Receiver,
			Callbacks: receiver.SessionCallbacks{Event: d.enqueue(events, token)},
		}
		session, err := d.options.SessionFactory(ctx, lifecycle)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !isRetryableReceiverError(err) {
				return fmt.Errorf("daemon: open receiver: %w", err)
			}
			d.logger.Printf("receiver unavailable; retrying: %v", safeError(err))
			if !wait(ctx, d.options.RetryInterval) {
				return nil
			}
			continue
		}
		if ctx.Err() != nil {
			if session != nil {
				_ = session.Close()
			}
			return nil
		}
		if session == nil {
			return errors.New("daemon: session factory returned nil session")
		}

		d.logSessionStarted(session)
		err = d.runSession(ctx, session, token, events)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		d.logger.Printf("receiver session ended; retrying")
		if !wait(ctx, d.options.RetryInterval) {
			return nil
		}
	}
}

type connectionToken struct{}

type lifecycleSignal struct {
	token *connectionToken
	event receiver.ChildEvent
}

func (d *Daemon) enqueue(events chan<- lifecycleSignal, token *connectionToken) func(receiver.ChildEvent) {
	return func(event receiver.ChildEvent) {
		select {
		case events <- lifecycleSignal{token: token, event: event}:
		default:
			d.logger.Printf("dropping receiver child lifecycle event %s: event queue full", event.Type)
		}
	}
}

type activeTarget struct {
	child        *receiver.ChildDevice
	configurator Configurator
	action       Action
}

func (d *Daemon) runSession(ctx context.Context, session Session, token *connectionToken, events <-chan lifecycleSignal) error {
	var active *activeTarget
	defer func() {
		if err := d.closeTarget(active); err != nil {
			d.logger.Printf("target cleanup failed: %v", safeError(err))
		}
		if err := session.Close(); err != nil {
			d.logger.Printf("receiver cleanup failed: %v", safeError(err))
		}
	}()

	for {
		var actionErrors <-chan error
		if active != nil && active.action != nil {
			actionErrors = active.action.Errors()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-session.Done():
			if err := session.Err(); err != nil {
				d.logger.Printf("receiver transport lost; reconnecting: %v", safeError(err))
			}
			return nil
		case err := <-actionErrors:
			if err == nil {
				continue
			}
			return fmt.Errorf("daemon: action output: %w", err)
		case signal := <-events:
			if signal.token != token {
				continue
			}
			var err error
			active, err = d.handleChildEvent(ctx, session, active, signal.event)
			if err != nil {
				return err
			}
		}
	}
}

func (d *Daemon) handleChildEvent(ctx context.Context, session Session, active *activeTarget, event receiver.ChildEvent) (*activeTarget, error) {
	child := event.Child
	metadata := event.Metadata
	if child != nil {
		metadata = child.Metadata()
	}
	if child == nil && metadata == (receiver.ChildMetadata{}) {
		return active, nil
	}

	switch event.Type {
	case receiver.ChildSleeping, receiver.ChildRemoved:
		if active != nil && (sameChild(active.child, child) || targetMatchesMetadata(metadata, d.settings.Device)) {
			if err := d.closeTarget(active); err != nil {
				d.logger.Printf("target state cleanup failed: %v", safeError(err))
			}
			active = nil
			d.logger.Printf("target device index %d is %s", metadata.WirelessIndex, event.Type)
		}
		return active, nil
	case receiver.ChildReady:
		if !targetMatchesEvent(child, metadata, d.settings.Device) {
			return active, nil
		}
		if active != nil && sameChild(active.child, child) {
			return active, nil
		}
		if active != nil {
			if err := d.closeTarget(active); err != nil {
				return nil, fmt.Errorf("daemon: replace target: %w", err)
			}
		}
		configured, err := d.activateTarget(ctx, session, child)
		if err != nil {
			return nil, err
		}
		return configured, nil
	default:
		return active, nil
	}
}

func (d *Daemon) activateTarget(ctx context.Context, session Session, child *receiver.ChildDevice) (*activeTarget, error) {
	index := byte(0)
	if child != nil {
		index = child.Metadata().WirelessIndex
	}
	d.logger.Printf("target device ready: index %d", index)
	configured, err := d.options.Configurator(ctx, session, d.settings)
	if err != nil {
		if isRecoverableDeviceError(err) {
			d.logger.Printf("target configuration deferred: %v", safeError(err))
			return nil, nil
		}
		return nil, fmt.Errorf("daemon: configure target: %w", err)
	}
	if configured == nil {
		return nil, errors.New("daemon: configurator returned nil configurator")
	}
	if err := configured.Apply(ctx); err != nil {
		if errors.Is(err, mxmaster.ErrNoFeature) {
			d.logger.Printf("optional target feature unavailable; continuing: %v", safeError(err))
		} else if isRecoverableDeviceError(err) {
			_ = configured.Close()
			d.logger.Printf("target configuration deferred: %v", safeError(err))
			return nil, nil
		} else {
			_ = configured.Close()
			return nil, fmt.Errorf("daemon: apply target configuration: %w", err)
		}
	}

	output, err := d.options.OutputFactory()
	if err != nil {
		_ = configured.Close()
		return nil, fmt.Errorf("daemon: open input output: %w", err)
	}
	action, err := configured.StartActions(ctx, output)
	if err != nil {
		_ = output.Close()
		_ = configured.Close()
		return nil, fmt.Errorf("daemon: start actions: %w", err)
	}
	if action == nil {
		_ = output.Close()
		_ = configured.Close()
		return nil, errors.New("daemon: configurator returned nil action handler")
	}
	return &activeTarget{child: child, configurator: configured, action: action}, nil
}

func (d *Daemon) closeTarget(active *activeTarget) error {
	if active == nil {
		return nil
	}
	var result error
	if active.action != nil {
		result = errors.Join(result, active.action.Reset())
		result = errors.Join(result, active.action.Stop())
	}
	if active.configurator != nil {
		result = errors.Join(result, active.configurator.Close())
	}
	return result
}

func (d *Daemon) logSessionStarted(session Session) {
	if receiverDevice := session.Receiver(); receiverDevice != nil {
		metadata := receiverDevice.Metadata()
		d.logger.Printf("receiver connected: type=%s path=%s", receiverDevice.Kind(), metadata.Path)
		return
	}
	d.logger.Printf("receiver connected")
}

func targetMatches(child *receiver.ChildDevice, target config.DeviceConfig) bool {
	if child == nil {
		return false
	}
	return targetMatchesMetadata(child.Metadata(), target)
}

func targetMatchesEvent(child *receiver.ChildDevice, metadata receiver.ChildMetadata, target config.DeviceConfig) bool {
	if child != nil {
		metadata = child.Metadata()
	}
	return targetMatchesMetadata(metadata, target)
}

func targetMatchesMetadata(metadata receiver.ChildMetadata, target config.DeviceConfig) bool {
	_, err := mxmaster.SelectMetadata([]receiver.ChildMetadata{metadata}, target)
	return err == nil
}

func sameChild(left, right *receiver.ChildDevice) bool {
	if left == nil || right == nil {
		return false
	}
	if left == right {
		return true
	}
	return left.Metadata().WirelessIndex == right.Metadata().WirelessIndex
}

func defaultSessionFactory(ctx context.Context, options receiver.LifecycleOptions) (Session, error) {
	return receiver.OpenSession(ctx, options)
}

func defaultConfiguratorFactory(ctx context.Context, session Session, settings config.Config) (Configurator, error) {
	receiverSession, ok := session.(*receiver.ReceiverSession)
	if !ok {
		return nil, errors.New("daemon: production configurator requires receiver session")
	}
	configured, err := mxmaster.NewConfigurator(ctx, receiverSession, settings)
	if err != nil {
		return nil, err
	}
	return configuratorAdapter{Configurator: configured}, nil
}

type configuratorAdapter struct {
	*mxmaster.Configurator
}

func (c configuratorAdapter) StartActions(ctx context.Context, output mxmaster.Output) (Action, error) {
	return c.Configurator.StartActions(ctx, output)
}

func defaultOutputFactory() (mxmaster.Output, error) {
	return uinput.Open(uinput.Options{Name: "logitechd virtual pointer"})
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func isRetryableReceiverError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, receiver.ErrNoReceiver) || errors.Is(err, os.ErrNotExist) {
		return true
	}
	return errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.ENXIO) || errors.Is(err, syscall.EIO)
}

func isRecoverableDeviceError(err error) bool {
	return errors.Is(err, mxmaster.ErrDeviceNotFound) ||
		errors.Is(err, hidpp.ErrClosedTransport) ||
		errors.Is(err, hidpp.ErrTimeout) ||
		errors.Is(err, os.ErrClosed)
}

func safeError(err error) error {
	if err == nil {
		return errors.New("unknown error")
	}
	return err
}
