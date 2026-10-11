package helper

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"sync"
	"time"
)

// ErrUnavailable marks a request the helper did not answer: no helper
// listening, a refused or dropped connection, or no answer in time. A
// refusal or a failed read is an answer, reported in Response.Error.
var ErrUnavailable = errors.New("the privileged helper is unavailable")

// Defaults for Client.
const (
	// DefaultTimeout bounds one request, dialling included. It is twice
	// the helper's own DefaultReadTimeout, so that a hung read is answered
	// as one before the client gives up on the helper.
	DefaultTimeout = 2 * DefaultReadTimeout
	// DefaultIdleTimeout is how long a connection is kept for the next
	// request. A collection's requests come in a burst, so one connection,
	// and one helper instance, serves the burst.
	DefaultIdleTimeout = 2 * time.Second
)

// Client sends requests to the helper listening on Socket, one at a time.
// It keeps its connection for IdleTimeout after a request, and drops it on
// any failure; the next request dials again. It is safe for concurrent use.
type Client struct {
	Socket      string
	Timeout     time.Duration // zero means DefaultTimeout
	IdleTimeout time.Duration // zero means DefaultIdleTimeout

	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
	idle *time.Timer

	dmi dmiCache
}

// Do sends req and returns the helper's answer. An error matches
// ErrUnavailable and never fs.ErrNotExist, even for a missing socket: that
// error means the host lacks a file.
func (c *Client) Do(req Request) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idle != nil {
		c.idle.Stop()
	}
	reused := c.conn != nil
	resp, err := c.roundTrip(req)
	if err != nil && reused && !isTimeout(err) {
		// The instance behind an idle connection may have exited, at
		// RuntimeMaxSec= or after a hung read: a fresh one decides.
		_ = c.closeLocked()
		resp, err = c.roundTrip(req)
	}
	if err != nil {
		_ = c.closeLocked()
		// %v, not %w: a dial error for a missing socket wraps
		// fs.ErrNotExist, which must not reach the caller.
		return Response{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if resp.Error == ErrCodeTimeout {
		// The helper exits after answering a hung read.
		_ = c.closeLocked()
	} else {
		c.idle = time.AfterFunc(cmp.Or(c.IdleTimeout, DefaultIdleTimeout), c.closeIdle)
	}
	return resp, nil
}

// Close stops the idle timer and closes the open connection, if any, so the
// helper's instance sees it end. The Client stays usable: a later request
// dials again.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idle != nil {
		c.idle.Stop()
	}
	return c.closeLocked()
}

func (c *Client) roundTrip(req Request) (Response, error) {
	timeout := cmp.Or(c.Timeout, DefaultTimeout)
	if c.conn == nil {
		conn, err := net.DialTimeout("unix", c.Socket, timeout)
		if err != nil {
			return Response{}, err
		}
		c.conn, c.r = conn, bufio.NewReader(conn)
	}
	if err := c.conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return Response{}, err
	}
	line, err := jsonLine(req)
	if err != nil {
		return Response{}, err
	}
	if _, err := c.conn.Write(line); err != nil {
		return Response{}, err
	}
	answer, err := c.r.ReadBytes('\n')
	if err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.Unmarshal(answer, &resp); err != nil {
		return Response{}, fmt.Errorf("malformed answer: %w", err)
	}
	return resp, nil
}

// jsonLine encodes v as one protocol line.
func jsonLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	return append(b, '\n'), err
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (c *Client) closeIdle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.closeLocked()
}

func (c *Client) closeLocked() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn, c.r = nil, nil
	return err
}

// FS presents the helper's operations as a file system; see NewFS. The DMI
// files are asked for once per Client; see dmiCache.
func (c *Client) FS() fs.FS { return c.fsOver(c.Do) }

// fsOver is FS over do, with the Client's DMI cache in front of it.
func (c *Client) fsOver(do func(Request) (Response, error)) fs.FS {
	return NewFS(c.dmi.wrap(do))
}
