package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/mdenesfe/wax/internal/binary"
	"github.com/mdenesfe/wax/internal/store"
)

// Errors returned by IQ requests.
var (
	ErrNotConnected = errors.New("client: not connected")
	ErrIQTimeout    = errors.New("client: iq timed out")
)

// IQError is an <iq type="error"> response.
type IQError struct {
	Code string
	Text string
	Node *binary.Node
}

func (e *IQError) Error() string { return fmt.Sprintf("iq error %s: %s", e.Code, e.Text) }

const (
	connectTimeout   = 30 * time.Second
	defaultIQTimeout = 30 * time.Second
	// handlerQueueSize decouples reading from handling. Handlers may block on
	// IQ round trips while the reader keeps delivering their responses.
	handlerQueueSize = 2048
)

// sessionResult describes how a connection ended.
type sessionResult struct {
	action      DisconnectAction
	err         error
	code        string
	detail      string
	loggedInFor time.Duration
}

// session is one connection attempt: one WebSocket, one Noise session.
type session struct {
	c   *Client
	log *slog.Logger
	dev *store.Device

	conn   *Conn
	ctx    context.Context
	cancel context.CancelFunc

	waitersMu sync.Mutex
	waiters   map[string]chan *binary.Node

	queue chan *binary.Node
	wg    sync.WaitGroup

	endOnce sync.Once
	result  sessionResult

	loggedInAt time.Time // set by the handler goroutine on <success>
	pairing    *qrState  // non-nil while showing QR codes
}

// runSession connects once and serves the connection until it ends.
func (c *Client) runSession(ctx context.Context, dev *store.Device) sessionResult {
	payload, err := c.payloadFor(dev)
	if err != nil {
		return sessionResult{action: ActionFatal, err: err}
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, connectTimeout)
	conn, err := Connect(dialCtx, ConnectOptions{
		URL: c.cfg.URL, HTTPClient: c.cfg.HTTPClient,
		NoiseKey: dev.NoiseKey, Payload: payload, Verifier: c.cfg.Verifier,
	})
	dialCancel()
	if err != nil {
		return sessionResult{action: ActionReconnect, err: fmt.Errorf("connect: %w", err)}
	}

	sctx, cancel := context.WithCancel(ctx)
	s := &session{
		c: c, dev: dev, conn: conn, ctx: sctx, cancel: cancel,
		log:     c.log.With("paired", dev.IsPaired()),
		waiters: make(map[string]chan *binary.Node),
		queue:   make(chan *binary.Node, handlerQueueSize),
	}
	s.log.Info("connected", "jid", binary.MaskJID(dev.JID))
	c.mu.Lock()
	c.current = s
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.current = nil
		c.mu.Unlock()
	}()

	s.wg.Add(2)
	go s.readLoop()
	go s.keepAliveLoop()
	s.handleLoop()

	// handleLoop returns once the session context is done.
	_ = conn.Close()
	s.wg.Wait()
	s.failWaiters()
	if s.result.action == ActionNone {
		// Cancelled from outside (ctx) without a recorded reason.
		s.result = sessionResult{action: ActionReconnect, err: ctx.Err()}
	}
	if !s.loggedInAt.IsZero() {
		s.result.loggedInFor = time.Since(s.loggedInAt)
	}
	return s.result
}

// end records why the session ends (first caller wins) and tears it down.
func (s *session) end(r sessionResult) {
	s.endOnce.Do(func() {
		s.result = r
		s.cancel()
	})
}

// goSafe runs fn on a tracked goroutine; a panic ends the session (and
// triggers a reconnect) instead of crashing the process.
func (s *session) goSafe(name string, fn func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.recoverInto(name)
		fn()
	}()
}

func (s *session) recoverInto(name string) {
	if r := recover(); r != nil {
		s.log.Error("panic in session goroutine", "goroutine", name, "panic", r, "stack", string(debug.Stack()))
		s.end(sessionResult{action: ActionReconnect, err: fmt.Errorf("panic in %s: %v", name, r)})
	}
}

func (s *session) readLoop() {
	defer s.wg.Done()
	defer s.recoverInto("reader")
	format := binary.FormatOptions{Mask: !s.c.cfg.DumpUnmasked}
	for {
		n, err := s.conn.ReadNode(s.ctx)
		var decErr *DecodeError
		if errors.As(err, &decErr) {
			s.log.Warn("skipping undecodable frame", "error", err)
			continue
		}
		if err != nil {
			s.end(sessionResult{action: ActionReconnect, err: fmt.Errorf("read: %w", err)})
			return
		}
		if s.c.cfg.DumpNodes {
			s.log.Debug("recv", "node", n.Format(format))
		}
		if s.deliverResponse(n) {
			continue
		}
		select {
		case s.queue <- n:
		case <-s.ctx.Done():
			return
		}
	}
}

// handleLoop processes non-response nodes in arrival order.
func (s *session) handleLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case n := <-s.queue:
			s.handleSafely(n)
		}
	}
}

func (s *session) handleSafely(n *binary.Node) {
	defer func() {
		// A bug in one handler must not take the session down: log and
		// continue with the next node.
		if r := recover(); r != nil {
			s.log.Error("panic while handling node", "tag", n.Tag, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	s.handleNode(n)
}

func (s *session) sendNode(n binary.Node) error {
	return s.conn.SendNode(s.ctx, n)
}

// sendIQ sends an IQ and waits for the response with the same id.
func (s *session) sendIQ(ctx context.Context, n binary.Node, timeout time.Duration) (*binary.Node, error) {
	if n.Attrs == nil {
		n.Attrs = binary.Attrs{}
	}
	id, _ := n.Attrs["id"].(string)
	if id == "" {
		id = s.conn.NextID()
		n.Attrs["id"] = id
	}
	if timeout <= 0 {
		timeout = defaultIQTimeout
	}
	ch := make(chan *binary.Node, 1)
	s.waitersMu.Lock()
	if s.waiters == nil {
		s.waitersMu.Unlock()
		return nil, ErrNotConnected
	}
	s.waiters[id] = ch
	s.waitersMu.Unlock()
	defer func() {
		s.waitersMu.Lock()
		if s.waiters != nil {
			delete(s.waiters, id)
		}
		s.waitersMu.Unlock()
	}()

	if err := s.sendNode(n); err != nil {
		return nil, err
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, ErrNotConnected
		}
		if typ, _ := resp.AttrString("type"); typ == "error" {
			e := &IQError{Node: resp}
			if en, ok := resp.Child("error"); ok {
				e.Code, _ = en.AttrString("code")
				e.Text, _ = en.AttrString("text")
			}
			return resp, e
		}
		return resp, nil
	case <-t.C:
		return nil, fmt.Errorf("%w: %s", ErrIQTimeout, id)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, ErrNotConnected
	}
}

// deliverResponse hands iq results/errors to the waiting sendIQ.
func (s *session) deliverResponse(n *binary.Node) bool {
	if n.Tag != "iq" {
		return false
	}
	typ, _ := n.AttrString("type")
	if typ != "result" && typ != "error" {
		return false
	}
	id, _ := n.AttrString("id")
	s.waitersMu.Lock()
	ch, ok := s.waiters[id]
	if ok {
		delete(s.waiters, id)
	}
	s.waitersMu.Unlock()
	if ok {
		ch <- n // buffered, never blocks
	}
	return ok
}

func (s *session) failWaiters() {
	s.waitersMu.Lock()
	defer s.waitersMu.Unlock()
	for _, ch := range s.waiters {
		close(ch)
	}
	s.waiters = nil
}
