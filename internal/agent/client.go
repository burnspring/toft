package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// Client is the platform's side of the socket.
type Client struct {
	dir  string
	http *http.Client
	// stream is the same transport with no overall timeout, for follows.
	stream *http.Client
}

// ErrNotFound is the agent saying it holds no workload by that name, or no
// live process behind that identity.
var ErrNotFound = errors.New("agent: not found")

// Unreachable reports whether err is nothing answering on the socket at all —
// no agent, rather than an agent that answered with a refusal.
func Unreachable(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

// ProtocolError is the agent refusing the caller's protocol, naming its own.
type ProtocolError struct{ Agent, Caller string }

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("the running agent speaks protocol %s and this platform %s", e.Agent, e.Caller)
}

// NewClient talks to the agent whose directory is dir. It does not dial: the
// first call does, and says plainly when nothing answers.
func NewClient(dir string) *Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", SocketPath(dir))
	}
	tr := &http.Transport{DialContext: dial, MaxIdleConns: 8, IdleConnTimeout: 30 * time.Second}
	return &Client{
		dir:    dir,
		http:   &http.Client{Transport: tr, Timeout: 2 * time.Minute},
		stream: &http.Client{Transport: &http.Transport{DialContext: dial}},
	}
}

// Dir is the agent's directory.
func (c *Client) Dir() string { return c.dir }

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set(ProtocolHeader, ProtocolVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("the shared-mode agent at %s did not answer: %w", SocketPath(c.dir), err)
	}
	defer resp.Body.Close()
	return decode(resp, out)
}

func decode(resp *http.Response, out any) error {
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusConflict:
		var e errorBody
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return &ProtocolError{Agent: e.Protocol, Caller: ProtocolVersion}
	case resp.StatusCode >= 300:
		var e errorBody
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Start places spec, replacing whatever held its name. An error is a start
// that did not happen: nothing is left running under the name.
func (c *Client) Start(ctx context.Context, spec Spec) (Status, error) {
	var st Status
	err := c.do(ctx, http.MethodPost, "/workloads/"+url.PathEscape(spec.Name), spec, &st)
	return st, err
}

// Stop halts a workload and keeps its name.
func (c *Client) Stop(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/workloads/"+url.PathEscape(name)+"/stop", nil, nil)
}

// Delete removes a workload, its output and its identity.
func (c *Client) Delete(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/workloads/"+url.PathEscape(name), nil, nil)
}

// List is every workload the agent holds.
func (c *Client) List(ctx context.Context) (List, error) {
	var l List
	err := c.do(ctx, http.MethodGet, "/workloads", nil, &l)
	return l, err
}

// Reexec asks the agent to replace itself with the binary on disk, keeping its
// pid and its workloads. The answer comes before the exec.
func (c *Client) Reexec(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/reexec", nil, nil)
}

// Get is one workload's status.
func (c *Client) Get(ctx context.Context, name string) (Status, error) {
	var st Status
	err := c.do(ctx, http.MethodGet, "/workloads/"+url.PathEscape(name), nil, &st)
	return st, err
}

// Verify asks who a token belongs to. ErrNotFound is a token no live process
// holds.
func (c *Client) Verify(ctx context.Context, token string) (VerifyResponse, error) {
	var v VerifyResponse
	err := c.do(ctx, http.MethodPost, "/identity/verify", VerifyRequest{Token: token}, &v)
	return v, err
}

// Follow streams one workload's events, or every workload's for an empty
// name, until ctx ends or the agent ends the stream.
func (c *Client) Follow(ctx context.Context, name string) (*Stream, error) {
	path := "/follow"
	if name != "" {
		path = "/workloads/" + url.PathEscape(name) + "/follow"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(ProtocolHeader, ProtocolVersion)
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the shared-mode agent did not answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, decode(resp, nil)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	return &Stream{body: resp.Body, sc: sc}, nil
}

// Stream is a follow.
type Stream struct {
	body io.ReadCloser
	sc   *bufio.Scanner
}

// Next is the next event, or io.EOF when the stream has ended.
func (s *Stream) Next() (Event, error) {
	for s.sc.Scan() {
		var e Event
		if err := json.Unmarshal(s.sc.Bytes(), &e); err != nil {
			continue
		}
		return e, nil
	}
	if err := s.sc.Err(); err != nil {
		return Event{}, err
	}
	return Event{}, io.EOF
}

func (s *Stream) Close() error { return s.body.Close() }

// Stats is the agent's own figures. An agent older than the route answers 404,
// which is ErrNotFound here.
func (c *Client) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	err := c.do(ctx, http.MethodGet, "/stats", nil, &s)
	return s, err
}
