package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"teleagent2api/internal/config"
)

// BuildTransport builds an *http.Transport that routes all egress traffic
// through the configured upstream proxy, in the style of the Resin
// proxy-pool gateway (https://github.com/Resinat/Resin).
//
// Supported schemes:
//
//	http://host:port            HTTP forward proxy
//	https://host:port           HTTP forward proxy over TLS
//	socks5://host:port          SOCKS5 proxy (hostname resolved by the proxy)
//	socks5h://host:port         same as socks5
//
// Proxy identity follows Resin's "Platform.Account:TOKEN" convention, either
// embedded in the URL userinfo:
//
//	http://Default.my-account:my-token@127.0.0.1:2260
//
// or composed from TELEAGENT_PROXY_PLATFORM / TELEAGENT_PROXY_ACCOUNT /
// TELEAGENT_PROXY_TOKEN. The Account segment gives sticky-session routing:
// the same business account is anchored to a stable outbound IP by Resin.
//
// Returns nil when no proxy is configured (caller should use the default
// transport).
func BuildTransport(cfg config.Config) (*http.Transport, error) {
	if !cfg.ProxyEnabled() {
		return nil, nil
	}

	raw := cfg.ProxyURL
	// Allow bare "host:port" shorthand → default to http forward proxy.
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse proxy url: %w", err)
	}

	// Compose Resin-style userinfo if not embedded in the URL.
	if u.User == nil || u.User.String() == "" {
		account := cfg.ProxyAccount
		if account == "" {
			// Stable default account derived from the deployment identity,
			// so a restart keeps the same sticky IP on the Resin side.
			account = "teleagent2api"
		}
		username := account
		if cfg.ProxyPlatform != "" {
			username = cfg.ProxyPlatform + "." + account
		}
		if cfg.ProxyToken != "" || username != "" {
			u.User = url.UserPassword(username, cfg.ProxyToken)
		}
	}

	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return &http.Transport{
			Proxy: http.ProxyURL(u),
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}, nil
	case "socks5", "socks5h":
		host := u.Host
		d := &socks5Dialer{
			proxyAddr: host,
			user:      u.User.Username(),
			pass:      "",
		}
		if p, ok := u.User.Password(); ok {
			d.pass = p
		}
		return &http.Transport{
			Proxy:                 nil, // handled by DialContext
			DialContext:           d.dialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (want http/https/socks5)", u.Scheme)
	}
}

// socks5Dialer is a minimal RFC 1928 SOCKS5 client with optional
// username/password authentication (RFC 1929), just enough to route
// *http.Transport egress through a SOCKS5 forward proxy.
type socks5Dialer struct {
	proxyAddr string
	user      string
	pass      string
}

func (d *socks5Dialer) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	// Only TCP makes sense through SOCKS5 CONNECT.
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("socks5: unsupported network %q", network)
	}

	var nd net.Dialer
	conn, err := nd.DialContext(ctx, "tcp", d.proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("socks5: dial proxy %s: %w", d.proxyAddr, err)
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if err := d.handshake(conn); err != nil {
		conn.Close()
		return nil, err
	}
	if err := d.connect(conn, addr); err != nil {
		conn.Close()
		return nil, err
	}

	// Clear the dial deadline; the caller manages per-request timeouts.
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// handshake negotiates the auth method; performs userpass auth if needed.
func (d *socks5Dialer) handshake(conn net.Conn) error {
	methods := []byte{0x00} // no authentication
	if d.user != "" {
		methods = []byte{0x00, 0x02} // prefer none, offer userpass
	}
	req := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5: write greeting: %w", err)
	}

	resp := make([]byte, 2)
	if _, err := readFull(conn, resp); err != nil {
		return fmt.Errorf("socks5: read greeting: %w", err)
	}
	if resp[0] != 0x05 {
		return fmt.Errorf("socks5: unexpected version %d", resp[0])
	}

	switch resp[1] {
	case 0x00:
		return nil
	case 0x02:
		if d.user == "" {
			return fmt.Errorf("socks5: proxy requires authentication")
		}
		return d.userpassAuth(conn)
	case 0xFF:
		return fmt.Errorf("socks5: no acceptable auth methods")
	default:
		return fmt.Errorf("socks5: unsupported auth method %d", resp[1])
	}
}

func (d *socks5Dialer) userpassAuth(conn net.Conn) error {
	if len(d.user) > 255 || len(d.pass) > 255 {
		return fmt.Errorf("socks5: credentials too long")
	}
	req := []byte{0x01, byte(len(d.user))}
	req = append(req, d.user...)
	req = append(req, byte(len(d.pass)))
	req = append(req, d.pass...)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5: write auth: %w", err)
	}

	resp := make([]byte, 2)
	if _, err := readFull(conn, resp); err != nil {
		return fmt.Errorf("socks5: read auth: %w", err)
	}
	if resp[1] != 0x00 {
		return fmt.Errorf("socks5: authentication failed (status %d)", resp[1])
	}
	return nil
}

// connect issues a CONNECT request for addr ("host:port" or "ip:port").
func (d *socks5Dialer) connect(conn net.Conn, addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("socks5: split addr %q: %w", addr, err)
	}
	port, err := net.LookupPort("tcp", portStr)
	if err != nil {
		return fmt.Errorf("socks5: lookup port %q: %w", portStr, err)
	}

	var req []byte
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append([]byte{0x05, 0x01, 0x00, 0x01}, v4...)
		} else {
			req = append([]byte{0x05, 0x01, 0x00, 0x04}, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("socks5: hostname too long")
		}
		req = append([]byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}, host...)
	}
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, uint16(port))
	req = append(req, portBuf...)

	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5: write connect: %w", err)
	}

	// Reply: VER REP RSV ATYP BND.ADDR BND.PORT
	head := make([]byte, 4)
	if _, err := readFull(conn, head); err != nil {
		return fmt.Errorf("socks5: read connect reply: %w", err)
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks5: connect failed (REP=%d)", head[1])
	}
	// Drain the bound address according to ATYP.
	var addrLen int
	switch head[3] {
	case 0x01:
		addrLen = net.IPv4len
	case 0x04:
		addrLen = net.IPv6len
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := readFull(conn, lenBuf); err != nil {
			return fmt.Errorf("socks5: read domain length: %w", err)
		}
		addrLen = int(lenBuf[0])
	default:
		return fmt.Errorf("socks5: unknown ATYP %d", head[3])
	}
	if _, err := readFull(conn, make([]byte, addrLen+2)); err != nil {
		return fmt.Errorf("socks5: drain bound addr: %w", err)
	}
	return nil
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
