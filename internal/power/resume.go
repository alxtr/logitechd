// Package power observes host power-state transitions exposed by systemd-logind.
package power

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	loginManagerPath         = dbus.ObjectPath("/org/freedesktop/login1")
	loginManagerSender       = "org.freedesktop.login1"
	prepareForSleep          = "org.freedesktop.login1.Manager.PrepareForSleep"
	loginManagerMember       = "PrepareForSleep"
	defaultReconnectInterval = 2 * time.Second
)

var reconnectInterval = defaultReconnectInterval

type signalConnection interface {
	AddMatchSignalContext(context.Context, ...dbus.MatchOption) error
	Signal(chan<- *dbus.Signal)
	RemoveSignal(chan<- *dbus.Signal)
	Context() context.Context
	Close() error
}

var connectSystemBus = func(ctx context.Context) (signalConnection, error) {
	return dbus.ConnectSystemBus(dbus.WithContext(ctx))
}

type subscription struct {
	conn    signalConnection
	signals chan *dbus.Signal
}

// WatchResumes returns a channel notified after systemd-logind reports that
// the host has resumed. Multiple pending notifications are coalesced. If the
// initial subscription fails, the returned channel remains active while the
// monitor retries and the error describes the temporary failure.
func WatchResumes(ctx context.Context) (<-chan struct{}, error) {
	if ctx == nil {
		return nil, errors.New("power: nil context")
	}

	resumes := make(chan struct{}, 1)
	current, err := subscribe(ctx)
	if err != nil {
		go monitor(ctx, nil, resumes, true)
		return resumes, err
	}

	go monitor(ctx, current, resumes, false)
	return resumes, nil
}

func subscribe(ctx context.Context) (*subscription, error) {
	conn, err := connectSystemBus(ctx)
	if err != nil {
		return nil, fmt.Errorf("power: connect to system bus: %w", err)
	}
	signals := make(chan *dbus.Signal, 8)
	conn.Signal(signals)
	match := []dbus.MatchOption{
		dbus.WithMatchSender(loginManagerSender),
		dbus.WithMatchObjectPath(loginManagerPath),
		dbus.WithMatchInterface("org.freedesktop.login1.Manager"),
		dbus.WithMatchMember(loginManagerMember),
	}
	if err := conn.AddMatchSignalContext(ctx, match...); err != nil {
		conn.RemoveSignal(signals)
		_ = conn.Close()
		return nil, fmt.Errorf("power: subscribe to logind sleep events: %w", err)
	}
	return &subscription{conn: conn, signals: signals}, nil
}

func monitor(ctx context.Context, current *subscription, resumes chan<- struct{}, recoveryNeeded bool) {
	defer close(resumes)
	for {
		if current != nil {
			if !forwardResumes(ctx, current, resumes) {
				current.close()
				return
			}
			current.close()
			current = nil
			recoveryNeeded = true
		}

		if !wait(ctx, reconnectInterval) {
			return
		}
		next, err := subscribe(ctx)
		if err != nil {
			continue
		}
		current = next
		if recoveryNeeded {
			notify(resumes)
			recoveryNeeded = false
		}
	}
}

func forwardResumes(ctx context.Context, current *subscription, resumes chan<- struct{}) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-current.conn.Context().Done():
			return ctx.Err() == nil
		case signal, ok := <-current.signals:
			if !ok {
				return ctx.Err() == nil
			}
			if !isResumeSignal(signal) {
				continue
			}
			notify(resumes)
		}
	}
}

func notify(resumes chan<- struct{}) {
	select {
	case resumes <- struct{}{}:
	default:
	}
}

func (s *subscription) close() {
	s.conn.RemoveSignal(s.signals)
	_ = s.conn.Close()
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

func isResumeSignal(signal *dbus.Signal) bool {
	if signal == nil || signal.Name != prepareForSleep || signal.Path != loginManagerPath || len(signal.Body) != 1 {
		return false
	}
	preparingForSleep, ok := signal.Body[0].(bool)
	return ok && !preparingForSleep
}
