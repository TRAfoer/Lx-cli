// Package player controls a dedicated mpv instance through its JSON IPC endpoint.
package player

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const commandTimeout = 5 * time.Second

// Event is an asynchronous mpv IPC notification. Raw preserves additional fields.
type Event struct {
	Event           string          `json:"event"`
	Reason          string          `json:"reason,omitempty"`
	PlaylistEntryID int64           `json:"playlist_entry_id,omitempty"`
	Raw             json.RawMessage `json:"-"`
}

// Client owns a connection to one explicit IPC endpoint. Command and Running
// never launch mpv; only Ensure does. A Client supports concurrent commands.
type Client struct {
	endpoint    string
	args        []string
	connectGate chan struct{}
	ensureGate  chan struct{}
	mu          sync.Mutex
	current     *session
	process     *ownedProcess
	sequence    atomic.Uint64
	events      chan Event
}

// New creates a client without opening IPC or starting a process.
func New(endpoint string, args []string) *Client {
	return &Client{
		endpoint: endpoint, args: append([]string(nil), args...),
		connectGate: make(chan struct{}, 1), ensureGate: make(chan struct{}, 1),
		events: make(chan Event, 128),
	}
}

// Events receives notifications from the live IPC connection. The channel stays
// open across disconnects. Consumers should select on their own context too.
// If a consumer falls more than 128 events behind, oldest notifications expire.
func (c *Client) Events() <-chan Event { return c.events }

// Command sends a JSON IPC command and waits only for its matching request_id.
// Calls without an earlier context deadline have a five-second upper bound.
func (c *Client) Command(ctx context.Context, command ...any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(command) == 0 {
		return nil, errors.New("mpv command is empty")
	}
	requestID := c.sequence.Add(1)
	request, err := json.Marshal(struct {
		Command   []any  `json:"command"`
		RequestID uint64 `json:"request_id"`
	}{command, requestID})
	if err != nil {
		return nil, fmt.Errorf("encode mpv command: %w", err)
	}
	s, err := c.connection(ctx)
	if err != nil {
		return nil, err
	}
	return s.command(ctx, requestID, append(request, '\n'))
}

// Running checks this endpoint with a bounded property query and never starts mpv.
func (c *Client) Running(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()
	_, err := c.Command(ctx, "get_property", "mpv-version")
	return err == nil
}

func (c *Client) connection(ctx context.Context) (*session, error) {
	if c.endpoint == "" {
		return nil, errors.New("mpv IPC endpoint is empty")
	}
	c.mu.Lock()
	s := c.current
	c.mu.Unlock()
	if s != nil && !s.closed() {
		return s, nil
	}
	if err := acquire(ctx, c.connectGate); err != nil {
		return nil, err
	}
	defer release(c.connectGate)
	c.mu.Lock()
	s = c.current
	c.mu.Unlock()
	if s != nil && !s.closed() {
		return s, nil
	}
	conn, err := dialIPC(ctx, c.endpoint)
	if err != nil {
		return nil, fmt.Errorf("connect mpv IPC: %w", err)
	}
	s = &session{conn: conn, pending: make(map[uint64]chan reply), done: make(chan struct{}), writeGate: make(chan struct{}, 1), publish: c.publish}
	c.mu.Lock()
	c.current = s
	c.mu.Unlock()
	go s.read()
	return s, nil
}

func (c *Client) publish(event Event) {
	select {
	case c.events <- event:
	default:
		select {
		case <-c.events:
		default:
		}
		select {
		case c.events <- event:
		default:
		}
	}
}

func acquire(ctx context.Context, gate chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func release(gate chan struct{}) { <-gate }

type reply struct {
	data json.RawMessage
	err  error
}

type session struct {
	conn      net.Conn
	mu        sync.Mutex
	pending   map[uint64]chan reply
	writeGate chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	publish   func(Event)
}

func (s *session) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *session) command(ctx context.Context, id uint64, message []byte) (json.RawMessage, error) {
	response := make(chan reply, 1)
	s.mu.Lock()
	if s.closed() {
		s.mu.Unlock()
		return nil, errors.New("mpv IPC disconnected")
	}
	s.pending[id] = response
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()
	if err := acquire(ctx, s.writeGate); err != nil {
		return nil, err
	}
	err := s.write(ctx, message)
	release(s.writeGate)
	if err != nil {
		return nil, err
	}
	select {
	case result := <-response:
		return result.data, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *session) write(ctx context.Context, message []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	if err := s.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	cancelDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = s.conn.SetWriteDeadline(time.Now()); close(cancelDone) })
	_, err := s.conn.Write(message)
	if !stop() {
		<-cancelDone
	}
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		s.shutdown(fmt.Errorf("write mpv IPC: %w", err))
		return err
	}
	return nil
}

func (s *session) read() {
	scanner := bufio.NewScanner(s.conn)
	scanner.Buffer(make([]byte, 4096), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var message struct {
			RequestID *uint64         `json:"request_id"`
			Error     string          `json:"error"`
			Data      json.RawMessage `json:"data"`
			Event
		}
		if err := json.Unmarshal(line, &message); err != nil {
			s.shutdown(fmt.Errorf("decode mpv IPC: %w", err))
			return
		}
		if message.Event.Event != "" {
			message.Raw = append(json.RawMessage(nil), line...)
			s.publish(message.Event)
			continue
		}
		if message.RequestID == nil {
			continue
		}
		result := reply{data: message.Data}
		if message.Error != "success" {
			result.err = fmt.Errorf("mpv: %s", message.Error)
		}
		s.mu.Lock()
		response := s.pending[*message.RequestID]
		if response != nil {
			select {
			case response <- result:
			default:
			}
		}
		s.mu.Unlock()
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	s.shutdown(fmt.Errorf("read mpv IPC: %w", err))
}

func (s *session) shutdown(err error) {
	s.closeOnce.Do(func() {
		_ = s.conn.Close()
		s.mu.Lock()
		close(s.done)
		for _, response := range s.pending {
			select {
			case response <- reply{err: err}:
			default:
			}
		}
		s.mu.Unlock()
	})
}
