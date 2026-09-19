package engine

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

// Only harvest callers opt into this extra hop. The HTTP transport still owns
// the inner dynamic proxy (including SOCKS authentication and remote DNS).
func harvestClient(dynamic, outer string) (*http.Client, error) {
	client, err := freshProbeClient(dynamic)
	if err != nil || outer == "" {
		return client, err
	}
	if dynamic == "" || validateProxy(outer) != nil {
		return nil, errors.New("invalid harvest chain")
	}
	u, err := url.Parse(outer)
	if err != nil {
		return nil, errors.New("invalid harvest chain")
	}
	var dial proxy.ContextDialer
	if u.Scheme == "http" || u.Scheme == "https" {
		dial = &connectDialer{url: u}
	} else {
		if u.Port() == "" {
			u.Host = net.JoinHostPort(u.Hostname(), "1080")
		}
		u.Scheme = "socks5"
		d, err := proxy.FromURL(u, &net.Dialer{Timeout: 15 * time.Second})
		if err != nil {
			return nil, errors.New("invalid harvest chain")
		}
		var ok bool
		dial, ok = d.(proxy.ContextDialer)
		if !ok {
			return nil, errors.New("harvest chain lacks cancellation")
		}
	}
	client.Transport = &harvestTransport{base: client.Transport.(*http.Transport), dial: dial}
	return client, nil
}

// net/http detaches its dial context from request cancellation. Fresh harvest
// tunnels have no reuse benefit; bind each dial to its owning request too.
type harvestTransport struct {
	base *http.Transport
	dial proxy.ContextDialer
}

func (t *harvestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr := t.base.Clone()
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		stop := context.AfterFunc(req.Context(), cancel)
		defer stop()
		return t.dial.DialContext(ctx, network, address)
	}
	defer tr.CloseIdleConnections()
	return tr.RoundTrip(req)
}

type connectDialer struct{ url *url.URL }

func (d *connectDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	port := d.url.Port()
	if port == "" {
		port = "80"
		if d.url.Scheme == "https" {
			port = "443"
		}
	}
	conn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(d.url.Hostname(), port))
	if err != nil {
		return nil, errors.New("front proxy connection failed")
	}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()
	raw := conn
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	if deadline, exists := ctx.Deadline(); exists {
		conn.SetDeadline(deadline)
	}
	if d.url.Scheme == "https" {
		t := tls.Client(conn, &tls.Config{ServerName: d.url.Hostname(), MinVersion: tls.VersionTLS12})
		if t.HandshakeContext(ctx) != nil {
			return nil, errors.New("front proxy TLS failed")
		}
		conn = t
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
	if d.url.User != nil {
		password, _ := d.url.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(d.url.User.Username()+":"+password)))
	}
	if req.Write(conn) != nil {
		return nil, errors.New("front proxy CONNECT failed")
	}
	reader := bufio.NewReader(io.LimitReader(conn, 64<<10))
	response, err := http.ReadResponse(reader, req)
	if err != nil || response.StatusCode != http.StatusOK {
		return nil, errors.New("front proxy CONNECT rejected")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	conn.SetDeadline(time.Time{})
	ok = true
	return &bufferedTunnel{Conn: conn, reader: reader}, nil
}

type bufferedTunnel struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedTunnel) Read(p []byte) (int, error) {
	if n := c.reader.Buffered(); n > 0 {
		if len(p) > n {
			p = p[:n]
		}
		return c.reader.Read(p)
	}
	return c.Conn.Read(p)
}
