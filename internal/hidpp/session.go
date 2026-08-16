package hidpp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

const (
	// DefaultTransactionTimeout bounds a transaction whose context has no
	// deadline. A caller-provided context deadline always takes precedence.
	DefaultTransactionTimeout = 2 * time.Second

	// maxTransportReportLength is the largest canonical HID++ report supported
	// by the report package. Transport implementations return one report per
	// ReadReport call.
	maxTransportReportLength = longReportLength
)

// Transport is the bidirectional report stream used by Session. Close must
// unblock an outstanding ReadReport; hidraw.Device provides this behavior.
type Transport interface {
	ReadReport([]byte) (int, error)
	WriteReport([]byte) error
	Close() error
}

// SessionOptions controls a Session's local transaction behavior.
type SessionOptions struct {
	// TransactionTimeout is used when an exchange context has no deadline.
	// Zero selects DefaultTransactionTimeout. Negative values are rejected.
	TransactionTimeout time.Duration
}

// Request describes a request that expects a response with ResponseSubID.
// The response must retain the request's device index and command/address
// byte. Report IDs are framing and do not form part of HID++ transaction
// identity.
type Request struct {
	Report        Report
	ResponseSubID byte
}

// Session owns one physical HID++ transport and dispatches its responses to
// concurrent logical transactions. Writes are serialized, while the reader
// remains shared so multiple device indexes can use one receiver later.
//
// The concurrency invariant is that this is the only reader and every request
// is matched using its device index plus protocol-specific command identity.
// Callers may issue receiver (0xff) and child requests concurrently; they do
// not use a transport-level lock or a second reader. The write gate protects
// report writes, and the pending table protects response ownership.
type Session struct {
	transport Transport
	timeout   time.Duration

	writeGate chan struct{}

	stateMu    sync.Mutex
	closed     bool
	terminal   error
	closeOnce  sync.Once
	closeError error

	pendingMu sync.Mutex
	pending   map[responseKey][]*pendingTransaction
	nextOrder uint64

	done chan struct{}

	reportHandlerMu  sync.RWMutex
	reportHandler    func(Report)
	subscriptions    map[uint64]func(Report)
	nextSubscription uint64
}

type responseKey struct {
	deviceIndex byte
	subID       byte
	address     byte
}

type pendingTransaction struct {
	key    responseKey
	order  uint64
	result chan transactionResult
}

type transactionResult struct {
	report Report
	err    error
}

// NewSession starts a shared HID++ response dispatcher for transport. At most
// one options value may be supplied; omitting it uses the default timeout.
func NewSession(transport Transport, options ...SessionOptions) (*Session, error) {
	if transport == nil {
		return nil, errors.New("hidpp: nil transport")
	}
	if len(options) > 1 {
		return nil, errors.New("hidpp: multiple session options")
	}
	var sessionOptions SessionOptions
	if len(options) == 1 {
		sessionOptions = options[0]
	}
	timeout := sessionOptions.TransactionTimeout
	if timeout == 0 {
		timeout = DefaultTransactionTimeout
	}
	if timeout < 0 {
		return nil, fmt.Errorf("hidpp: invalid transaction timeout %s", timeout)
	}

	s := &Session{
		transport:     transport,
		timeout:       timeout,
		writeGate:     make(chan struct{}, 1),
		pending:       make(map[responseKey][]*pendingTransaction),
		done:          make(chan struct{}),
		subscriptions: make(map[uint64]func(Report)),
	}
	s.writeGate <- struct{}{}
	go s.readLoop()
	return s, nil
}

// NewDefaultSession is a convenience constructor using the default timeout.
func NewDefaultSession(transport Transport) (*Session, error) {
	return NewSession(transport, SessionOptions{})
}

// SetReportHandler installs a callback for reports that are not consumed by a
// waiting Exchange. HID++ notifications are unsolicited reports, so this is
// the hook used by higher layers while keeping one reader for one transport.
// The callback runs on the session reader goroutine and should return
// promptly. Passing nil removes the current callback.
func (s *Session) SetReportHandler(handler func(Report)) {
	if s == nil {
		return
	}
	s.reportHandlerMu.Lock()
	s.reportHandler = handler
	s.reportHandlerMu.Unlock()
}

// SubscribeReport registers a callback for reports not consumed by an
// Exchange. It is useful for independent child-device event consumers sharing
// this session. The returned function is idempotent and removes the callback.
// The callback runs on the session reader goroutine and must return promptly.
func (s *Session) SubscribeReport(handler func(Report)) func() {
	if s == nil || handler == nil {
		return func() {}
	}
	s.reportHandlerMu.Lock()
	s.nextSubscription++
	id := s.nextSubscription
	s.subscriptions[id] = handler
	s.reportHandlerMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.reportHandlerMu.Lock()
			delete(s.subscriptions, id)
			s.reportHandlerMu.Unlock()
		})
	}
}

// Exchange writes request and waits for its matching response. Unrelated
// reports are left available to their own transaction (or ignored when no
// transaction is waiting for them).
func (s *Session) Exchange(ctx context.Context, request Request) (Report, error) {
	if ctx == nil {
		return Report{}, errors.New("hidpp: nil context")
	}
	if request.Report.Type == ReportTypeShort && request.ResponseSubID == RegisterErrorSubID {
		return Report{}, &UnsupportedError{
			Operation: "response sub-ID",
			Detail:    "error reports cannot be transaction responses",
		}
	}
	if request.Report.Type != ReportTypeShort && request.ResponseSubID == FeatureErrorSubID {
		return Report{}, &UnsupportedError{
			Operation: "response feature index",
			Detail:    "HID++ 2.0 error reports cannot be transaction responses",
		}
	}
	data, err := Build(request.Report)
	if err != nil {
		return Report{}, err
	}

	effectiveContext, cancel := s.transactionContext(ctx)
	defer cancel()
	if err := effectiveContext.Err(); err != nil {
		return Report{}, contextError("exchange", ctx, err)
	}

	transaction := &pendingTransaction{
		key: responseKey{
			deviceIndex: request.Report.DeviceIndex,
			subID:       request.ResponseSubID,
			address:     request.Report.CommandByte(),
		},
		result: make(chan transactionResult, 1),
	}
	s.addPending(transaction)

	if err := s.write(effectiveContext, data); err != nil {
		s.removePending(transaction)
		return Report{}, err
	}

	select {
	case result := <-transaction.result:
		return result.report, result.err
	case <-effectiveContext.Done():
		s.removePending(transaction)
		return Report{}, contextError("exchange", ctx, effectiveContext.Err())
	case <-s.done:
		s.removePending(transaction)
		return Report{}, s.closedError()
	}
}

// Send writes a report without registering a response waiter. It is used for
// commands whose caller deliberately does not expect a response.
func (s *Session) Send(ctx context.Context, report Report) error {
	if ctx == nil {
		return errors.New("hidpp: nil context")
	}
	data, err := Build(report)
	if err != nil {
		return err
	}
	effectiveContext, cancel := s.transactionContext(ctx)
	defer cancel()
	if err := effectiveContext.Err(); err != nil {
		return contextError("send", ctx, err)
	}
	return s.write(effectiveContext, data)
}

// Close stops the dispatcher, unblocks waiters, and closes the underlying
// transport. It is safe to call more than once.
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.markClosed(os.ErrClosed)
		s.closeError = s.transport.Close()
		if isTransportClosed(s.closeError) {
			s.closeError = &ClosedTransportError{Cause: s.closeError}
		}
	})
	<-s.done
	return s.closeError
}

func (s *Session) transactionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok || s.timeout == 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, s.timeout)
}

func (s *Session) write(ctx context.Context, data []byte) error {
	select {
	case <-s.writeGate:
	case <-ctx.Done():
		return contextError("write", ctx, ctx.Err())
	case <-s.done:
		return s.closedError()
	}
	defer func() { s.writeGate <- struct{}{} }()

	if err := s.isClosed(); err != nil {
		return err
	}
	if err := s.transport.WriteReport(data); err != nil {
		if isTransportClosed(err) {
			closed := &ClosedTransportError{Cause: err}
			s.markClosed(err)
			return closed
		}
		return fmt.Errorf("hidpp: write request: %w", err)
	}
	return nil
}

func (s *Session) readLoop() {
	defer close(s.done)
	buffer := make([]byte, maxTransportReportLength)
	for {
		n, err := s.transport.ReadReport(buffer)
		if err != nil {
			closed := &ClosedTransportError{Cause: err}
			s.markClosed(err)
			s.failPending(closed)
			return
		}
		if n < 0 || n > len(buffer) {
			s.failPending(malformedResponse(buffer, fmt.Errorf("invalid read count %d", n)))
			continue
		}
		data := buffer[:n]
		report, err := Parse(data)
		if err != nil {
			s.failPending(malformedResponse(data, err))
			continue
		}
		s.dispatch(report)
	}
}

func (s *Session) dispatch(report Report) {
	if report.Type == ReportTypeShort && report.SubID == RegisterErrorSubID {
		protocolError, err := protocolErrorFromReport(report)
		if err != nil {
			s.failPending(err)
			return
		}
		if transaction := s.takePendingError(report.DeviceIndex, protocolError.RequestSubID, protocolError.RequestAddress); transaction != nil {
			transaction.result <- transactionResult{err: protocolError}
		}
		return
	}
	if report.Type != ReportTypeShort && report.SubID == FeatureErrorSubID {
		protocolError, err := protocolErrorFromFeatureReport(report)
		if err != nil {
			s.failPending(err)
			return
		}
		if transaction := s.takePendingFeatureError(report.DeviceIndex, protocolError.RequestSubID, protocolError.RequestAddress); transaction != nil {
			transaction.result <- transactionResult{err: protocolError}
		}
		return
	}

	key := responseKey{
		deviceIndex: report.DeviceIndex,
		subID:       report.SubID,
		address:     report.CommandByte(),
	}
	if transaction := s.takePending(key); transaction != nil {
		transaction.result <- transactionResult{report: report}
		return
	}

	s.reportHandlerMu.RLock()
	handler := s.reportHandler
	callbacks := make([]func(Report), 0, len(s.subscriptions))
	for _, callback := range s.subscriptions {
		callbacks = append(callbacks, callback)
	}
	s.reportHandlerMu.RUnlock()
	if handler != nil {
		handler(report)
	}
	for _, callback := range callbacks {
		callback(report)
	}
}

func protocolErrorFromReport(report Report) (*ProtocolError, error) {
	if len(report.Parameters) < 3 {
		return nil, malformedResponse(nil, fmt.Errorf("HID++ error report has %d parameters, need 3", len(report.Parameters)))
	}
	parameters := append([]byte(nil), report.Parameters...)
	return &ProtocolError{
		DeviceIndex:    report.DeviceIndex,
		RequestSubID:   parameters[0],
		RequestAddress: parameters[1],
		Code:           parameters[2],
		Parameters:     parameters,
	}, nil
}

// HID++ 2.0 errors use feature index 0xff. The command byte contains the
// feature index from the failed request and parameter zero contains its
// function/software-ID byte; parameter one is the error code.
func protocolErrorFromFeatureReport(report Report) (*ProtocolError, error) {
	if len(report.Parameters) < 2 {
		return nil, malformedResponse(nil, fmt.Errorf("HID++ 2.0 error report has %d parameters, need 2", len(report.Parameters)))
	}
	parameters := append([]byte(nil), report.Parameters...)
	return &ProtocolError{
		DeviceIndex:    report.DeviceIndex,
		RequestSubID:   report.CommandByte(),
		RequestAddress: parameters[0],
		Code:           parameters[1],
		Parameters:     parameters,
	}, nil
}

func (s *Session) addPending(transaction *pendingTransaction) {
	s.pendingMu.Lock()
	s.nextOrder++
	transaction.order = s.nextOrder
	s.pending[transaction.key] = append(s.pending[transaction.key], transaction)
	s.pendingMu.Unlock()
}

func (s *Session) takePending(key responseKey) *pendingTransaction {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	transactions := s.pending[key]
	if len(transactions) == 0 {
		return nil
	}
	transaction := transactions[0]
	if len(transactions) == 1 {
		delete(s.pending, key)
	} else {
		s.pending[key] = transactions[1:]
	}
	return transaction
}

// HID++ 1.0 error reports are short reports even when the failed request was
// long. Match their device/sub-ID/address identity without requiring the
// report format to be the same as the request.
func (s *Session) takePendingError(deviceIndex, subID, address byte) *pendingTransaction {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	var selected *pendingTransaction
	var selectedKey responseKey
	for key, transactions := range s.pending {
		if key.deviceIndex != deviceIndex || key.subID != subID || key.address != address || len(transactions) == 0 {
			continue
		}
		candidate := transactions[0]
		if selected == nil || candidate.order < selected.order {
			selected = candidate
			selectedKey = key
		}
	}
	if selected == nil {
		return nil
	}
	transactions := s.pending[selectedKey]
	if len(transactions) == 1 {
		delete(s.pending, selectedKey)
	} else {
		s.pending[selectedKey] = transactions[1:]
	}
	return selected
}

func (s *Session) takePendingFeatureError(deviceIndex, featureIndex, command byte) *pendingTransaction {
	return s.takePending(responseKey{deviceIndex: deviceIndex, subID: featureIndex, address: command})
}

func (s *Session) removePending(target *pendingTransaction) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	transactions := s.pending[target.key]
	for i, transaction := range transactions {
		if transaction == target {
			transactions = append(transactions[:i], transactions[i+1:]...)
			if len(transactions) == 0 {
				delete(s.pending, target.key)
			} else {
				s.pending[target.key] = transactions
			}
			return
		}
	}
}

func (s *Session) failPending(err error) {
	s.pendingMu.Lock()
	transactions := make([]*pendingTransaction, 0)
	for key, waiting := range s.pending {
		transactions = append(transactions, waiting...)
		delete(s.pending, key)
	}
	s.pendingMu.Unlock()
	for _, transaction := range transactions {
		transaction.result <- transactionResult{err: err}
	}
}

func (s *Session) markClosed(cause error) {
	s.stateMu.Lock()
	if !s.closed {
		s.closed = true
		s.terminal = cause
	}
	s.stateMu.Unlock()
}

func (s *Session) isClosed() error {
	s.stateMu.Lock()
	closed := s.closed
	terminal := s.terminal
	s.stateMu.Unlock()
	if !closed {
		return nil
	}
	return &ClosedTransportError{Cause: terminal}
}

func (s *Session) closedError() error {
	if err := s.isClosed(); err != nil {
		return err
	}
	return &ClosedTransportError{Cause: os.ErrClosed}
}

func contextError(operation string, original context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &TimeoutError{Operation: operation, Cause: err}
	}
	if original != nil && errors.Is(original.Err(), context.DeadlineExceeded) {
		return &TimeoutError{Operation: operation, Cause: original.Err()}
	}
	return err
}

func isTransportClosed(err error) bool {
	return errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
}
