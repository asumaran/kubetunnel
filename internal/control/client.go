package control

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/asumaran/kubetunnel/internal/logging"
)

// Client talks to the daemon over its unix socket.
type Client struct {
	socket string
	http   *http.Client
}

func NewClient(socket string) *Client {
	return &Client{
		socket: socket,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
			Timeout: 10 * time.Second,
		},
	}
}

func (c *Client) url(path string, q url.Values) string {
	u := "http://unix" + path
	if q != nil {
		u += "?" + q.Encode()
	}
	return u
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var e Error
		_ = json.Unmarshal(body, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(body))
		}
		return nil, fmt.Errorf("daemon returned %d: %s", resp.StatusCode, e.Error)
	}
	return resp, nil
}

func (c *Client) Status() (*StatusResponse, error) {
	req, _ := http.NewRequest("GET", c.url("/status", nil), nil)
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Reload() error {
	req, _ := http.NewRequest("POST", c.url("/reload", nil), nil)
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) Restart(name string) error {
	q := url.Values{"name": []string{name}}
	req, _ := http.NewRequest("POST", c.url("/restart", q), nil)
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) Shutdown() error {
	req, _ := http.NewRequest("POST", c.url("/shutdown", nil), nil)
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Logs fetches the last `tail` entries matching filter. name can be empty.
func (c *Client) Logs(name, filter string, tail int) (*LogResponse, error) {
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	if filter != "" {
		q.Set("filter", filter)
	}
	if tail > 0 {
		q.Set("tail", fmt.Sprint(tail))
	}
	req, _ := http.NewRequest("GET", c.url("/logs", q), nil)
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out LogResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StreamLogs opens an SSE stream of log entries. Returns a channel that emits
// entries until ctx is canceled.
func (c *Client) StreamLogs(ctx context.Context, name, filter string) (<-chan logging.Entry, error) {
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	if filter != "" {
		q.Set("filter", filter)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.url("/logs/stream", q), nil)
	if err != nil {
		return nil, err
	}
	// Streaming client — no global timeout.
	sc := &http.Client{
		Transport: c.http.Transport,
	}
	resp, err := sc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("stream returned %d: %s", resp.StatusCode, body)
	}

	out := make(chan logging.Entry, 64)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			var e logging.Entry
			if err := json.Unmarshal([]byte(payload), &e); err != nil {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case out <- e:
			}
		}
	}()
	return out, nil
}

// StreamStatus opens the /events SSE stream and returns a channel of status
// snapshots.
func (c *Client) StreamStatus(ctx context.Context) (<-chan StatusResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.url("/events", nil), nil)
	if err != nil {
		return nil, err
	}
	sc := &http.Client{Transport: c.http.Transport}
	resp, err := sc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("stream returned %d: %s", resp.StatusCode, body)
	}
	out := make(chan StatusResponse, 8)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			var s StatusResponse
			if err := json.Unmarshal([]byte(payload), &s); err != nil {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case out <- s:
			}
		}
	}()
	return out, nil
}
