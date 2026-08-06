package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// socksDialer connects through a SOCKS5 proxy, which in every deployment this
// ships to means Tor.
//
// Hand-rolled rather than taken from golang.org/x/net, for two reasons. The
// obvious one is that this is sixty lines against a new dependency in the tree of
// a program that handles a security decision. The one that actually decided it is
// below: the behaviour that matters here is a non-default in that package, and a
// non-default is something a future edit can quietly drop.
type socksDialer struct {
	// Address is the proxy's own host:port, reached directly.
	Address string
	// Dialer reaches the proxy. Substituted by tests; nil uses the default.
	Dialer *net.Dialer
}

// SOCKS5 wire constants, from RFC 1928.
const (
	socksVersion  = 0x05
	socksNoAuth   = 0x00
	socksConnect  = 0x01
	socksReserved = 0x00

	socksAddrIPv4   = 0x01
	socksAddrDomain = 0x03
	socksAddrIPv6   = 0x04

	socksSucceeded = 0x00
)

// maxDomainLen is the longest hostname the protocol can carry: the length is a
// single byte.
const maxDomainLen = 255

// DialContext opens a connection to addr through the proxy.
//
// **The hostname is sent to the proxy unresolved, always.** This is the whole
// reason the function exists. Resolving locally and connecting to the resulting
// address would send a DNS query for the download host from the user's own
// network — which defeats the point of proxying the transfer at all, since the
// query names the destination just as clearly as the connection would have, and
// goes to a resolver that is very often their internet provider's.
//
// Go's own net.Dialer cannot express this: any address it is given, it resolves.
// So the address is parsed here and handed over as a domain name whenever it is
// not already a literal IP.
func (d socksDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("bootstrap: the proxy cannot carry %s connections", network)
	}

	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: %q is not a host and port: %w", addr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("bootstrap: %q is not a usable port", portText)
	}
	if len(host) > maxDomainLen {
		return nil, fmt.Errorf("bootstrap: host name is too long for the proxy protocol")
	}

	dialer := d.Dialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	conn, err := dialer.DialContext(ctx, "tcp", d.Address)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: cannot reach the proxy at %s: %w", d.Address, err)
	}

	// The handshake gets a deadline of its own, and the context can cut it short.
	// Without this, a proxy that accepts connections and then says nothing would
	// hold the transfer open indefinitely — which is exactly how a Tor daemon
	// behaves while it is still bootstrapping, and it is the state a freshly
	// installed appliance is in for the first few minutes.
	deadline := time.Now().Add(handshakeTimeout)
	if fromCtx, ok := ctx.Deadline(); ok && fromCtx.Before(deadline) {
		deadline = fromCtx
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })

	if err := socksHandshake(conn, host, port); err != nil {
		stop()
		_ = conn.Close()
		return nil, err
	}

	// Cleared, because from here the transfer's own timeouts apply and they are
	// far longer than any handshake should be given. A deadline left in place
	// would abort a download that was running perfectly.
	_ = conn.SetDeadline(time.Time{})

	// stop() reports false when the cancellation already ran, which means the
	// connection has been closed underneath a handshake that happened to succeed
	// first. Returning it would hand back a dead socket.
	if !stop() {
		return nil, ctx.Err()
	}
	return conn, nil
}

// handshakeTimeout bounds the exchange with the proxy. Generous for a few
// round-trips to a local daemon, short enough that a misconfigured proxy address
// is reported rather than waited on.
const handshakeTimeout = 30 * time.Second

func socksHandshake(conn net.Conn, host string, port int) error {
	// Greeting: one method offered, and it is "none". Tor accepts this, and
	// offering username/password would invite a proxy to ask for credentials
	// this has no way to supply.
	if _, err := conn.Write([]byte{socksVersion, 1, socksNoAuth}); err != nil {
		return fmt.Errorf("bootstrap: greeting the proxy: %w", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("bootstrap: the proxy did not answer: %w", err)
	}
	if reply[0] != socksVersion {
		return fmt.Errorf("bootstrap: the proxy speaks version %d, not 5", reply[0])
	}
	if reply[1] != socksNoAuth {
		return errors.New("bootstrap: the proxy wants authentication, which is not " +
			"how a Tor proxy is normally configured — check the proxy address")
	}

	request := []byte{socksVersion, socksConnect, socksReserved}
	switch ip := net.ParseIP(host); {
	case ip == nil:
		// The length fits a byte because DialContext refused anything longer
		// than maxDomainLen before reaching here.
		//nolint:gosec // bounded above by maxDomainLen, which is 255.
		request = append(request, socksAddrDomain, byte(len(host)))
		request = append(request, host...)
	case ip.To4() != nil:
		request = append(request, socksAddrIPv4)
		request = append(request, ip.To4()...)
	default:
		request = append(request, socksAddrIPv6)
		request = append(request, ip.To16()...)
	}
	// Likewise bounded: DialContext rejected anything outside 1..65535.
	//nolint:gosec // a validated port always fits in two bytes.
	request = append(request, byte(port>>8), byte(port))

	if _, err := conn.Write(request); err != nil {
		return fmt.Errorf("bootstrap: asking the proxy to connect: %w", err)
	}
	return readSocksReply(conn)
}

// readSocksReply consumes the connect reply, including its variable-length bound
// address, which must be drained even though nothing here uses it — anything left
// unread would be delivered to the caller as the first bytes of the response.
func readSocksReply(conn net.Conn) error {
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("bootstrap: the proxy did not answer the connect request: %w", err)
	}
	if head[0] != socksVersion {
		return fmt.Errorf("bootstrap: the proxy replied with version %d, not 5", head[0])
	}
	if head[1] != socksSucceeded {
		return fmt.Errorf("bootstrap: the proxy refused the connection: %s",
			socksFailure(head[1]))
	}

	var addrLen int
	switch head[3] {
	case socksAddrIPv4:
		addrLen = net.IPv4len
	case socksAddrIPv6:
		addrLen = net.IPv6len
	case socksAddrDomain:
		size := make([]byte, 1)
		if _, err := io.ReadFull(conn, size); err != nil {
			return fmt.Errorf("bootstrap: reading the proxy's reply: %w", err)
		}
		addrLen = int(size[0])
	default:
		return fmt.Errorf("bootstrap: the proxy replied with address type %d, "+
			"which is not one this understands", head[3])
	}

	// The address plus a two-byte port.
	if _, err := io.ReadFull(conn, make([]byte, addrLen+2)); err != nil {
		return fmt.Errorf("bootstrap: reading the proxy's reply: %w", err)
	}
	return nil
}

// socksFailure translates a reply code into something worth reading.
//
// The generic codes are worth spelling out because the two most common ones here
// have very specific causes: a Tor daemon still bootstrapping refuses with
// "network unreachable", and a blocked exit policy refuses with "not allowed".
// Both look identical to "the proxy is broken" without this.
func socksFailure(code byte) string {
	switch code {
	case 0x01:
		return "the proxy had a general failure"
	case 0x02:
		return "the proxy's rules do not allow this connection"
	case 0x03:
		return "the network was unreachable, which for Tor usually means it is " +
			"still starting up"
	case 0x04:
		return "the host was unreachable"
	case 0x05:
		return "the connection was refused"
	case 0x06:
		return "the connection timed out"
	case 0x07:
		return "the proxy does not support this kind of request"
	case 0x08:
		return "the proxy does not support this kind of address"
	default:
		return fmt.Sprintf("the proxy reported code %d", code)
	}
}
