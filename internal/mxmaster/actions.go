package mxmaster

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/atremb/logitechd/internal/config"
	"github.com/atremb/logitechd/internal/uinput"
)

const defaultGestureThreshold = 20

// Output is the portable sink used by ActionHandler. It is an alias so
// callers do not need to know which concrete output package is used.
type Output = uinput.Output

// FakeOutput is a hardware-free output useful to package users and tests.
type FakeOutput = uinput.FakeOutput

// NewFakeOutput constructs the portable test sink without exposing the Linux
// device implementation to action-processing callers.
func NewFakeOutput() *FakeOutput { return uinput.NewFakeOutput() }

// ActionDispatcher is retained as a descriptive alias for integrations that
// prefer the word dispatcher over handler.
type ActionDispatcher = ActionHandler

// NewActionDispatcher is an alias for NewActionHandler.
func NewActionDispatcher(settings config.Config, output Output) (*ActionHandler, error) {
	return NewActionHandler(settings, output)
}

type relativeAction struct {
	axis  uint16
	value int32
}

type actionBinding struct {
	keys     []uint16
	relative *relativeAction
	command  bool
}

// ActionHandler serializes decoded device events and output reports. It can be
// driven synchronously with Handle or attached to a Configurator.Events stream
// with Start. Physical pointer reports are not accepted by this type; only
// diverted button, raw-XY, and configured wheel events reach Output.
type ActionHandler struct {
	mu       sync.Mutex
	output   Output
	settings config.Config

	buttonBindings  map[config.CID]actionBinding
	gestureBindings map[gestureDirection]actionBinding
	pressed         map[config.CID]bool
	gesture         gestureDirection
	gestureDX       int32
	gestureDY       int32

	held      map[uint16]bool
	heldOrder []uint16
	wheelRest int32
	thumbRest int32

	started       bool
	stopRequested bool
	closed        bool
	stop          chan struct{}
	done          chan struct{}
	lastErr       error
	errors        chan error
}

// NewActionHandler validates and compiles all configured action values before
// returning. No uinput node is opened here; the output lifecycle remains
// explicit for the future daemon owner.
func NewActionHandler(settings config.Config, output Output) (*ActionHandler, error) {
	if output == nil {
		return nil, errors.New("mxmaster: nil action output")
	}
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	h := &ActionHandler{
		output:          output,
		settings:        settings,
		buttonBindings:  make(map[config.CID]actionBinding),
		gestureBindings: make(map[gestureDirection]actionBinding),
		pressed:         make(map[config.CID]bool),
		held:            make(map[uint16]bool),
		errors:          make(chan error, 1),
	}
	for cid, action := range settings.Buttons {
		binding, err := compileAction(action)
		if err != nil {
			return nil, fmt.Errorf("mxmaster: button %s: %w", cid, err)
		}
		h.buttonBindings[cid] = binding
	}
	if gestures := configuredGestures(settings); gestures != nil {
		for direction, action := range gestureActions(*gestures) {
			if action.Action == "" {
				continue
			}
			binding, err := compileAction(action)
			if err != nil {
				return nil, fmt.Errorf("mxmaster: gesture %s: %w", direction, err)
			}
			h.gestureBindings[direction] = binding
		}
	}
	return h, nil
}

// NewHandler is a short constructor alias.
func NewHandler(settings config.Config, output Output) (*ActionHandler, error) {
	return NewActionHandler(settings, output)
}

// Errors receives asynchronous errors from a handler started with Start. The
// channel is buffered and never blocks event processing.
func (h *ActionHandler) Errors() <-chan error {
	if h == nil {
		return nil
	}
	return h.errors
}

// Start attaches the handler to an event stream. Start returns after the
// worker is installed; Stop or Close joins it and releases all held controls.
func (h *ActionHandler) Start(ctx context.Context, events <-chan InputEvent) error {
	if ctx == nil {
		return errors.New("mxmaster: nil action context")
	}
	if events == nil {
		return errors.New("mxmaster: nil action event stream")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errors.New("mxmaster: action handler is closed")
	}
	if h.started {
		return errors.New("mxmaster: action handler is already started")
	}
	h.started = true
	h.stopRequested = false
	h.stop = make(chan struct{})
	h.done = make(chan struct{})
	stop, done := h.stop, h.done
	go h.run(ctx, events, stop, done)
	return nil
}

func (h *ActionHandler) run(ctx context.Context, events <-chan InputEvent, stop <-chan struct{}, done chan<- struct{}) {
	defer func() {
		h.mu.Lock()
		h.started = false
		h.mu.Unlock()
		close(done)
	}()
	for {
		select {
		case <-ctx.Done():
			h.reportError(h.releaseAll())
			return
		case <-stop:
			h.reportError(h.releaseAll())
			return
		case event, ok := <-events:
			if !ok {
				h.reportError(h.releaseAll())
				return
			}
			if err := h.Handle(event); err != nil {
				h.reportError(err)
				h.reportError(h.releaseAll())
				return
			}
		}
	}
}

// Handle processes one decoded event synchronously. Calls from multiple
// goroutines are serialized, including all output writes and synchronization.
func (h *ActionHandler) Handle(event InputEvent) error {
	if h == nil {
		return errors.New("mxmaster: nil action handler")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errors.New("mxmaster: action handler is closed")
	}
	return h.handleLocked(event)
}

// Dispatch is an explicit alias for Handle.
func (h *ActionHandler) Dispatch(event InputEvent) error { return h.Handle(event) }

// Reconfigure atomically replaces actions after releasing the old state. It
// is safe to call while a Start worker is running because Handle is serialized
// on the same mutex.
func (h *ActionHandler) Reconfigure(settings config.Config) error {
	if h == nil {
		return errors.New("mxmaster: nil action handler")
	}
	compiled, err := compileSettings(settings)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errors.New("mxmaster: action handler is closed")
	}
	if err := h.releaseAllLocked(); err != nil {
		return err
	}
	h.settings = settings
	h.buttonBindings = compiled.buttons
	h.gestureBindings = compiled.gestures
	h.pressed = make(map[config.CID]bool)
	h.gesture = gestureNone
	h.gestureDX, h.gestureDY = 0, 0
	h.wheelRest, h.thumbRest = 0, 0
	return nil
}

// Reset releases all diverted controls and clears wheel and gesture remainder
// state while keeping the handler and output usable. Lifecycle owners should
// call it when a child sleeps, disconnects, or is otherwise replaced.
func (h *ActionHandler) Reset() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	return h.releaseAllLocked()
}

// ReleaseAll is an explicit lifecycle alias for Reset.
func (h *ActionHandler) ReleaseAll() error { return h.Reset() }

// Stop stops the worker, releases held keys/buttons, and destroys the output.
// It is safe to call repeatedly and concurrently.
func (h *ActionHandler) Stop() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	if h.closed {
		err := h.lastErr
		h.mu.Unlock()
		return err
	}
	stop, done := h.stop, h.done
	if h.started && !h.stopRequested {
		h.stopRequested = true
		close(stop)
	}
	h.mu.Unlock()
	if done != nil {
		<-done
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return h.lastErr
	}
	cleanupErr := h.releaseAllLocked()
	h.closed = true
	closeErr := h.output.Close()
	h.lastErr = errors.Join(h.lastErr, cleanupErr, closeErr)
	return h.lastErr
}

// Close is the lifecycle-friendly spelling of Stop.
func (h *ActionHandler) Close() error { return h.Stop() }

// ReleaseGesture releases the currently held gesture binding without changing
// button state. It is useful when a future lifecycle owner receives an
// explicit gesture-end notification.
func (h *ActionHandler) ReleaseGesture() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.gesture = gestureNone
	h.gestureDX, h.gestureDY = 0, 0
	return h.reconcileLocked(nil)
}

func (h *ActionHandler) handleLocked(event InputEvent) error {
	switch event.Kind {
	case EventButtons:
		return h.handleButtonsLocked(event.Buttons)
	case EventRawXY:
		return h.handleRawXYLocked(event.RawXY)
	case EventWheel:
		return h.handleWheelLocked(event.Wheel)
	case EventThumbWheel:
		return h.handleThumbWheelLocked(event.ThumbWheel)
	default:
		return nil
	}
}

func (h *ActionHandler) handleButtonsLocked(event ControlButtonEvent) error {
	current := make(map[config.CID]bool, len(event.ControlIDs))
	for _, id := range event.ControlIDs {
		current[config.CID(id)] = true
	}
	ids := make([]int, 0, len(current))
	for id := range current {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	var extra []uinput.Event
	for _, value := range ids {
		cid := config.CID(value)
		if current[cid] && !h.pressed[cid] {
			binding := h.buttonBindings[cid]
			if binding.command {
				return fmt.Errorf("mxmaster: command action for button %s is not executable", cid)
			}
			if binding.relative != nil {
				extra = append(extra, relativeEvent(*binding.relative))
			}
		}
	}
	h.pressed = current
	return h.reconcileLocked(extra)
}

func (h *ActionHandler) handleRawXYLocked(event RawXYEvent) error {
	if event.Release || (event.DX == 0 && event.DY == 0) {
		h.gesture = gestureNone
		h.gestureDX, h.gestureDY = 0, 0
		return h.reconcileLocked(nil)
	}
	if len(h.gestureBindings) == 0 {
		return nil
	}
	h.gestureDX += int32(event.DX)
	h.gestureDY += int32(event.DY)
	threshold := defaultGestureThreshold
	if gestures := configuredGestures(h.settings); gestures != nil && gestures.Threshold > 0 {
		threshold = gestures.Threshold
	}
	if h.gesture == gestureNone && abs32(h.gestureDX)+abs32(h.gestureDY) < int32(threshold) {
		return nil
	}
	direction := directionFor(event.DX, event.DY)
	if direction == gestureNone {
		return nil
	}
	if h.gesture == gestureNone {
		h.gestureDX, h.gestureDY = 0, 0
	}
	if direction == h.gesture {
		return nil
	}
	binding := h.gestureBindings[direction]
	if binding.command {
		return fmt.Errorf("mxmaster: command action for gesture %s is not executable", direction)
	}
	var extra []uinput.Event
	if binding.relative != nil {
		extra = append(extra, relativeEvent(*binding.relative))
	}
	h.gesture = direction
	return h.reconcileLocked(extra)
}

func (h *ActionHandler) handleWheelLocked(event WheelEvent) error {
	if !h.wheelToOutput() {
		return nil
	}
	if event.Delta == 0 {
		return nil
	}
	if !event.HighResolution {
		h.wheelRest = 0
		return h.reconcileLocked([]uinput.Event{{Type: uinput.EV_REL, Code: uinput.REL_WHEEL, Value: int32(event.Delta)}})
	}
	return h.handleHiResLocked(&h.wheelRest, uinput.REL_WHEEL_HI_RES, uinput.REL_WHEEL, int32(event.Delta))
}

func (h *ActionHandler) handleThumbWheelLocked(event ThumbEvent) error {
	if !h.thumbToOutput() || event.Delta == 0 {
		return nil
	}
	return h.handleHiResLocked(&h.thumbRest, uinput.REL_HWHEEL_HI_RES, uinput.REL_HWHEEL, int32(event.Delta))
}

func (h *ActionHandler) handleHiResLocked(remainder *int32, hiAxis, lowAxis uint16, delta int32) error {
	var events []uinput.Event
	if supports(h.output, uinput.EV_REL, hiAxis) {
		events = append(events, uinput.Event{Type: uinput.EV_REL, Code: hiAxis, Value: delta})
	}
	whole := consumeHiRes(remainder, delta)
	if whole != 0 && supports(h.output, uinput.EV_REL, lowAxis) {
		events = append(events, uinput.Event{Type: uinput.EV_REL, Code: lowAxis, Value: whole})
	}
	return h.reconcileLocked(events)
}

func (h *ActionHandler) wheelToOutput() bool {
	setting := h.settings.HiResScroll
	return setting != nil && (setting.Target == "uinput" || setting.Target == "os") && (setting.Enabled == nil || *setting.Enabled)
}

func (h *ActionHandler) thumbToOutput() bool {
	setting := h.settings.ThumbWheel
	return setting != nil && setting.Divert != nil && *setting.Divert
}

func (h *ActionHandler) reconcileLocked(extra []uinput.Event) error {
	desiredOrder := h.desiredOrderLocked()
	desired := make(map[uint16]bool, len(desiredOrder))
	for _, key := range desiredOrder {
		desired[key] = true
	}
	var events []uinput.Event
	for offset := len(h.heldOrder) - 1; offset >= 0; offset-- {
		key := h.heldOrder[offset]
		if !desired[key] {
			events = append(events, uinput.Event{Type: uinput.EV_KEY, Code: key, Value: 0})
		}
	}
	events = append(events, extra...)
	for _, key := range desiredOrder {
		if !h.held[key] {
			events = append(events, uinput.Event{Type: uinput.EV_KEY, Code: key, Value: 1})
		}
	}
	if len(events) == 0 {
		return nil
	}
	actual := make(map[uint16]bool, len(h.held))
	for key, held := range h.held {
		actual[key] = held
	}
	actualOrder := append([]uint16(nil), h.heldOrder...)
	for _, event := range events {
		if err := h.output.Emit(event); err != nil {
			h.held = actual
			h.heldOrder = actualOrder
			return fmt.Errorf("mxmaster: emit input event type 0x%x code 0x%x: %w", event.Type, event.Code, err)
		}
		if event.Type != uinput.EV_KEY {
			continue
		}
		if event.Value == 0 {
			delete(actual, event.Code)
			for offset, key := range actualOrder {
				if key == event.Code {
					actualOrder = append(actualOrder[:offset], actualOrder[offset+1:]...)
					break
				}
			}
		} else if !actual[event.Code] {
			actual[event.Code] = true
			actualOrder = append(actualOrder, event.Code)
		}
	}
	if err := h.output.Sync(); err != nil {
		h.held = actual
		h.heldOrder = actualOrder
		return fmt.Errorf("mxmaster: synchronize input report: %w", err)
	}
	h.held = desired
	h.heldOrder = desiredOrder
	return nil
}

func (h *ActionHandler) desiredOrderLocked() []uint16 {
	ids := make([]int, 0, len(h.pressed))
	for cid, pressed := range h.pressed {
		if pressed {
			ids = append(ids, int(cid))
		}
	}
	sort.Ints(ids)
	result := make([]uint16, 0, len(ids)*2+2)
	seen := make(map[uint16]bool)
	for _, value := range ids {
		for _, key := range h.buttonBindings[config.CID(value)].keys {
			if !seen[key] {
				seen[key] = true
				result = append(result, key)
			}
		}
	}
	if binding, ok := h.gestureBindings[h.gesture]; ok {
		for _, key := range binding.keys {
			if !seen[key] {
				seen[key] = true
				result = append(result, key)
			}
		}
	}
	return result
}

func (h *ActionHandler) releaseAll() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.releaseAllLocked()
}

func (h *ActionHandler) releaseAllLocked() error {
	h.pressed = make(map[config.CID]bool)
	h.gesture = gestureNone
	h.gestureDX, h.gestureDY = 0, 0
	h.wheelRest, h.thumbRest = 0, 0
	err := h.reconcileLocked(nil)
	if err == nil {
		h.held = make(map[uint16]bool)
		h.heldOrder = nil
	}
	return err
}

func (h *ActionHandler) reportError(err error) {
	if err == nil {
		return
	}
	h.mu.Lock()
	h.lastErr = errors.Join(h.lastErr, err)
	h.mu.Unlock()
	select {
	case h.errors <- err:
	default:
	}
}

type gestureDirection uint8

const (
	gestureNone gestureDirection = iota
	gestureLeft
	gestureRight
	gestureUp
	gestureDown
)

func (d gestureDirection) String() string {
	switch d {
	case gestureLeft:
		return "left"
	case gestureRight:
		return "right"
	case gestureUp:
		return "up"
	case gestureDown:
		return "down"
	default:
		return "none"
	}
}

func directionFor(dx, dy int16) gestureDirection {
	if dx == 0 && dy == 0 {
		return gestureNone
	}
	if abs32(int32(dx)) >= abs32(int32(dy)) {
		if dx < 0 {
			return gestureLeft
		}
		return gestureRight
	}
	if dy < 0 {
		return gestureUp
	}
	return gestureDown
}

func abs32(value int32) int32 {
	if value < 0 {
		return -value
	}
	return value
}

// HiResRemainder is a small reusable accumulator for Linux's 120 units per
// wheel click convention.
type HiResRemainder struct{ value int32 }

func (r *HiResRemainder) Add(delta int32) int32 {
	if r == nil {
		return 0
	}
	return consumeHiRes(&r.value, delta)
}

func (r *HiResRemainder) Remainder() int32 {
	if r == nil {
		return 0
	}
	return r.value
}

func consumeHiRes(remainder *int32, delta int32) int32 {
	if remainder == nil {
		return 0
	}
	value := int64(*remainder) + int64(delta)
	whole := int32(value / 120)
	value -= int64(whole) * 120
	*remainder = int32(value)
	return whole
}

func supports(output Output, kind uinput.EventType, code uint16) bool {
	if capabilities, ok := output.(uinput.Capabilities); ok {
		return capabilities.Supports(kind, code)
	}
	return true
}

func relativeEvent(action relativeAction) uinput.Event {
	return uinput.Event{Type: uinput.EV_REL, Code: action.axis, Value: action.value}
}

type compiledSettings struct {
	buttons  map[config.CID]actionBinding
	gestures map[gestureDirection]actionBinding
}

func compileSettings(settings config.Config) (compiledSettings, error) {
	if err := settings.Validate(); err != nil {
		return compiledSettings{}, err
	}
	result := compiledSettings{
		buttons:  make(map[config.CID]actionBinding),
		gestures: make(map[gestureDirection]actionBinding),
	}
	for cid, action := range settings.Buttons {
		binding, err := compileAction(action)
		if err != nil {
			return compiledSettings{}, fmt.Errorf("mxmaster: button %s: %w", cid, err)
		}
		result.buttons[cid] = binding
	}
	if gestures := configuredGestures(settings); gestures != nil {
		for direction, action := range gestureActions(*gestures) {
			if action.Action == "" {
				continue
			}
			binding, err := compileAction(action)
			if err != nil {
				return compiledSettings{}, fmt.Errorf("mxmaster: gesture %s: %w", direction, err)
			}
			result.gestures[direction] = binding
		}
	}
	return result, nil
}

func gestureActions(value config.GestureConfig) map[gestureDirection]config.ActionSpec {
	return map[gestureDirection]config.ActionSpec{
		gestureLeft: value.Left, gestureRight: value.Right,
		gestureUp: value.Up, gestureDown: value.Down,
	}
}

func configuredGestures(settings config.Config) *config.GestureConfig {
	if settings.Gestures != nil {
		return settings.Gestures
	}
	return settings.RawXY
}

func compileAction(action config.ActionSpec) (actionBinding, error) {
	name := strings.ToLower(strings.TrimSpace(action.Action))
	switch name {
	case "", "none":
		return actionBinding{}, nil
	case "back":
		return keyBinding("KEY_BACK")
	case "forward":
		return keyBinding("KEY_FORWARD")
	case "middle":
		return keyBinding("BTN_MIDDLE")
	case "copy":
		return keyBinding("KEY_LEFTCTRL", "KEY_C")
	case "paste":
		return keyBinding("KEY_LEFTCTRL", "KEY_V")
	case "key", "button":
		parts := splitNames(action.Value)
		if len(parts) == 0 {
			return actionBinding{}, errors.New("action value has no keys")
		}
		return keyBinding(parts...)
	case "command":
		if strings.TrimSpace(action.Value) == "" {
			return actionBinding{}, errors.New("command action has an empty value")
		}
		return actionBinding{command: true}, nil
	case "scroll":
		return scrollBinding(action.Value)
	case "axis", "relative":
		return axisBinding(action.Value)
	default:
		return actionBinding{}, fmt.Errorf("unknown action %q", action.Action)
	}
}

func keyBinding(names ...string) (actionBinding, error) {
	keys := make([]uint16, 0, len(names))
	seen := make(map[uint16]bool)
	for _, name := range names {
		code, err := uinput.KeyCode(name)
		if err != nil {
			return actionBinding{}, err
		}
		if !seen[code] {
			seen[code] = true
			keys = append(keys, code)
		}
	}
	return actionBinding{keys: keys}, nil
}

func scrollBinding(value string) (actionBinding, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "up":
		return actionBinding{relative: &relativeAction{axis: uinput.REL_WHEEL, value: 1}}, nil
	case "down":
		return actionBinding{relative: &relativeAction{axis: uinput.REL_WHEEL, value: -1}}, nil
	case "left":
		return actionBinding{relative: &relativeAction{axis: uinput.REL_HWHEEL, value: -1}}, nil
	case "right":
		return actionBinding{relative: &relativeAction{axis: uinput.REL_HWHEEL, value: 1}}, nil
	default:
		return actionBinding{}, fmt.Errorf("invalid scroll direction %q", value)
	}
}

func axisBinding(value string) (actionBinding, error) {
	parts := strings.FieldsFunc(strings.TrimSpace(value), func(r rune) bool { return r == ':' || r == '=' })
	if len(parts) == 0 || len(parts) > 2 {
		return actionBinding{}, fmt.Errorf("relative action %q must be AXIS[:value]", value)
	}
	axisName := strings.ToUpper(strings.TrimSpace(parts[0]))
	axis := map[string]uint16{
		"X": uinput.REL_X, "REL_X": uinput.REL_X,
		"Y": uinput.REL_Y, "REL_Y": uinput.REL_Y,
		"WHEEL": uinput.REL_WHEEL, "REL_WHEEL": uinput.REL_WHEEL,
		"HWHEEL": uinput.REL_HWHEEL, "REL_HWHEEL": uinput.REL_HWHEEL,
		"WHEEL_HI_RES": uinput.REL_WHEEL_HI_RES, "REL_WHEEL_HI_RES": uinput.REL_WHEEL_HI_RES,
		"HWHEEL_HI_RES": uinput.REL_HWHEEL_HI_RES, "REL_HWHEEL_HI_RES": uinput.REL_HWHEEL_HI_RES,
	}
	code, ok := axis[axisName]
	if !ok {
		return actionBinding{}, fmt.Errorf("unknown relative axis %q", parts[0])
	}
	amount := int64(1)
	if len(parts) == 2 {
		parsed, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 32)
		if err != nil || parsed == 0 {
			return actionBinding{}, fmt.Errorf("invalid relative value %q", parts[1])
		}
		amount = parsed
	}
	return actionBinding{relative: &relativeAction{axis: code, value: int32(amount)}}, nil
}

func splitNames(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool { return r == '+' || r == ',' || r == ' ' || r == '\t' || r == '\n' })
}
