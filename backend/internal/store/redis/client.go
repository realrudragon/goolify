package redis

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrNil         = errors.New("redis: nil")
	ErrUnavailable = errors.New("redis: unavailable")
)

// Client is a lightweight, zero-dependency RESP2 Redis client scoped to Coolify's Go namespace.
type Client struct {
	addr     string
	password string
	db       int
	prefix   string
	timeout  time.Duration

	mu   sync.Mutex
	conn net.Conn
	rw   *bufio.ReadWriter
}

// ParseAndNew creates a Redis Client from a URL like redis://[:pass@]host:6379/0.
func ParseAndNew(rawURL string, prefix string) (*Client, error) {
	if prefix == "" {
		prefix = "coolify:go:v1:"
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	host := u.Host
	if host == "" {
		host = "127.0.0.1:6379"
	}
	if !strings.Contains(host, ":") {
		host += ":6379"
	}
	var pass string
	if u.User != nil {
		pass, _ = u.User.Password()
	}
	db := 0
	if p := strings.TrimPrefix(u.Path, "/"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v >= 0 {
			db = v
		}
	}
	return &Client{
		addr:     host,
		password: pass,
		db:       db,
		prefix:   prefix,
		timeout:  3 * time.Second,
	}, nil
}

func (c *Client) Prefix() string { return c.prefix }

func (c *Client) Key(k string) string {
	if strings.HasPrefix(k, c.prefix) {
		return k
	}
	return c.prefix + k
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		c.rw = nil
		return err
	}
	return nil
}

func (c *Client) ensureConn(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	d := net.Dialer{Timeout: c.timeout}
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	c.conn = conn
	c.rw = rw
	if c.password != "" {
		if _, err := c.doLocked(ctx, "AUTH", c.password); err != nil {
			_ = conn.Close()
			c.conn = nil
			c.rw = nil
			return err
		}
	}
	if c.db > 0 {
		if _, err := c.doLocked(ctx, "SELECT", strconv.Itoa(c.db)); err != nil {
			_ = conn.Close()
			c.conn = nil
			c.rw = nil
			return err
		}
	}
	return nil
}

func (c *Client) Do(ctx context.Context, args ...string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureConn(ctx); err != nil {
		return nil, err
	}
	val, err := c.doLocked(ctx, args...)
	if err != nil && !errors.Is(err, ErrNil) {
		// Drop broken connection so next call reconnects cleanly.
		if c.conn != nil {
			_ = c.conn.Close()
			c.conn = nil
			c.rw = nil
		}
	}
	return val, err
}

func (c *Client) doLocked(ctx context.Context, args ...string) (any, error) {
	deadline := time.Now().Add(c.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.conn.SetDeadline(deadline)

	if err := writeCommand(c.rw.Writer, args...); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := c.rw.Flush(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return readReply(c.rw.Reader)
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.Do(ctx, "PING")
	return err
}

func (c *Client) Incr(ctx context.Context, key string) (int64, error) {
	res, err := c.Do(ctx, "INCR", c.Key(key))
	if err != nil {
		return 0, err
	}
	v, ok := res.(int64)
	if !ok {
		return 0, fmt.Errorf("unexpected INCR reply type %T", res)
	}
	return v, nil
}

func (c *Client) Get(ctx context.Context, key string) (string, error) {
	res, err := c.Do(ctx, "GET", c.Key(key))
	if err != nil {
		return "", err
	}
	s, ok := res.(string)
	if !ok {
		return "", fmt.Errorf("unexpected GET reply type %T", res)
	}
	return s, nil
}

func (c *Client) GetInt64(ctx context.Context, key string) (int64, error) {
	s, err := c.Get(ctx, key)
	if errors.Is(err, ErrNil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(s, 10, 64)
}

func (c *Client) Set(ctx context.Context, key string, val string, ttl time.Duration) error {
	if ttl > 0 {
		ms := strconv.FormatInt(ttl.Milliseconds(), 10)
		_, err := c.Do(ctx, "SET", c.Key(key), val, "PX", ms)
		return err
	}
	_, err := c.Do(ctx, "SET", c.Key(key), val)
	return err
}

func (c *Client) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	args := make([]string, 0, len(keys)+1)
	args = append(args, "DEL")
	for _, k := range keys {
		args = append(args, c.Key(k))
	}
	_, err := c.Do(ctx, args...)
	return err
}

// Eval runs a Lua script atomically in Redis (used for version-checked CAS cache writes).
func (c *Client) Eval(ctx context.Context, script string, keys []string, argv ...string) (any, error) {
	args := make([]string, 0, 3+len(keys)+len(argv))
	args = append(args, "EVAL", script, strconv.Itoa(len(keys)))
	for _, k := range keys {
		args = append(args, c.Key(k))
	}
	args = append(args, argv...)
	return c.Do(ctx, args...)
}

func (c *Client) Publish(ctx context.Context, channel string, payload string) error {
	_, err := c.Do(ctx, "PUBLISH", c.Key(channel), payload)
	return err
}

// FlushNamespace deletes ONLY keys matching this client's prefix (never FLUSHDB).
func (c *Client) FlushNamespace(ctx context.Context) (int, error) {
	cursor := "0"
	deleted := 0
	pattern := c.prefix + "*"
	for {
		res, err := c.Do(ctx, "SCAN", cursor, "MATCH", pattern, "COUNT", "200")
		if err != nil {
			return deleted, err
		}
		arr, ok := res.([]any)
		if !ok || len(arr) != 2 {
			return deleted, fmt.Errorf("unexpected SCAN reply")
		}
		cursor, _ = arr[0].(string)
		keysRaw, _ := arr[1].([]any)
		if len(keysRaw) > 0 {
			delArgs := make([]string, 0, len(keysRaw)+1)
			delArgs = append(delArgs, "DEL")
			for _, kr := range keysRaw {
				if ks, ok := kr.(string); ok {
					delArgs = append(delArgs, ks)
				}
			}
			if len(delArgs) > 1 {
				if _, err := c.Do(ctx, delArgs...); err != nil {
					return deleted, err
				}
				deleted += len(delArgs) - 1
			}
		}
		if cursor == "0" || cursor == "" {
			break
		}
	}
	return deleted, nil
}

func writeCommand(w *bufio.Writer, args ...string) error {
	if _, err := fmt.Fprintf(w, "*%d\r\n", len(args)); err != nil {
		return err
	}
	for _, a := range args {
		if _, err := fmt.Fprintf(w, "$%d\r\n%s\r\n", len(a), a); err != nil {
			return err
		}
	}
	return nil
}

func readReply(r *bufio.Reader) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	line = strings.TrimSuffix(line, "\r\n")
	if len(line) == 0 {
		return nil, ErrUnavailable
	}
	prefix := line[0]
	body := line[1:]
	switch prefix {
	case '+':
		return body, nil
	case '-':
		return nil, errors.New("redis error: " + body)
	case ':':
		return strconv.ParseInt(body, 10, 64)
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, ErrNil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(body)
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, ErrNil
		}
		out := make([]any, 0, n)
		for i := 0; i < n; i++ {
			item, err := readReply(r)
			if errors.Is(err, ErrNil) {
				out = append(out, nil)
				continue
			}
			if err != nil {
				return nil, err
			}
			out = append(out, item)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unexpected RESP prefix %q", prefix)
	}
}
