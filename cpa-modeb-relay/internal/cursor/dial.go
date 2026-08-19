package cursor

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// dialTLSViaEnvProxy opens a TLS connection to addr, tunneling through the
// HTTPS_PROXY/HTTP_PROXY CONNECT proxy when one is configured for the target.
// Deployments that block direct egress (relay-egress-guard) route all Cursor
// traffic through a loopback CONNECT proxy this way.
func dialTLSViaEnvProxy(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
	proxyURL, err := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
	if err != nil || proxyURL == nil {
		d := &tls.Dialer{Config: cfg}
		return d.DialContext(ctx, network, addr)
	}

	proxyAddr := proxyURL.Host
	if proxyURL.Port() == "" {
		port := "80"
		if proxyURL.Scheme == "https" {
			port = "443"
		}
		proxyAddr = net.JoinHostPort(proxyURL.Hostname(), port)
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("cursor: dial proxy %s: %w", proxyAddr, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	connectReq := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if user := proxyURL.User; user != nil {
		password, _ := user.Password()
		token := base64.StdEncoding.EncodeToString([]byte(user.Username() + ":" + password))
		connectReq.Header.Set("Proxy-Authorization", "Basic "+token)
	}
	if err = connectReq.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("cursor: proxy CONNECT write: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("cursor: proxy CONNECT read: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("cursor: proxy CONNECT %s: %s", addr, resp.Status)
	}
	var tunnel net.Conn = conn
	if br.Buffered() > 0 {
		tunnel = &bufferedConn{Conn: conn, reader: br}
	}
	_ = conn.SetDeadline(time.Time{})

	tlsConn := tls.Client(tunnel, cfg)
	if err = tlsConn.HandshakeContext(ctx); err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("cursor: tls handshake via proxy: %w", err)
	}
	return tlsConn, nil
}

// bufferedConn drains bytes the CONNECT response reader may have buffered
// before handing the stream to TLS.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if c.reader != nil && c.reader.Buffered() > 0 {
		return c.reader.Read(p)
	}
	return c.Conn.Read(p)
}
