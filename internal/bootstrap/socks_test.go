package bootstrap

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// socksOpts bends the fake proxy into the shapes a real one takes.
//
// **Fixed at construction, never afterwards.** The accept loop reads these from
// its own goroutine, and setting one on a proxy that is already listening is a
// data race — which the race detector duly found, in exactly the test that
// needed the flag most.
type socksOpts struct {
	// refuseWith, when non-zero, is the reply code sent instead of success.
	refuseWith byte
	// demandAuth answers the greeting with a method the dialer cannot do.
	demandAuth bool
	// replyAddrType is what the success reply claims. Zero means IPv4.
	replyAddrType byte
	// silent accepts the connection and then says nothing.
	silent bool
}

// fakeSocks is just enough of RFC 1928 to observe what the dialer asks for.
type fakeSocks struct {
	listener net.Listener
	opts     socksOpts

	mu sync.Mutex
	// requested is the address as the proxy was asked for it, which is the whole
	// point of the test: a hostname here means the name was never resolved
	// locally.
	requested []string

	// closed ends a silent handler at cleanup, so nothing here has to sleep to
	// simulate a proxy that never answers.
	closed chan struct{}
}

func newFakeSocks(t *testing.T, opts socksOpts) *fakeSocks {
	t.Helper()

	if opts.replyAddrType == 0 {
		opts.replyAddrType = socksAddrIPv4
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSocks{
		listener: ln,
		opts:     opts,
		closed:   make(chan struct{}),
	}
	t.Cleanup(func() {
		close(f.closed)
		_ = ln.Close()
	})

	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go f.handle(conn)
		}
	}()
	return f
}

func (f *fakeSocks) addr() string { return f.listener.Addr().String() }

func (f *fakeSocks) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requested...)
}

func (f *fakeSocks) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	if f.opts.silent {
		// Accept and say nothing, which is how a Tor daemon behaves while it is
		// still bootstrapping. Held open until the test tears down rather than
		// for a fixed time: a test that waits out a duration is slow now and
		// flaky later.
		<-f.closed
		return
	}

	greeting := make([]byte, 2)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		return
	}
	methods := make([]byte, greeting[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if f.opts.demandAuth {
		_, _ = conn.Write([]byte{socksVersion, 0x02})
		return
	}
	if _, err := conn.Write([]byte{socksVersion, socksNoAuth}); err != nil {
		return
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}
	var host string
	switch head[3] {
	case socksAddrDomain:
		size := make([]byte, 1)
		if _, err := io.ReadFull(conn, size); err != nil {
			return
		}
		name := make([]byte, size[0])
		if _, err := io.ReadFull(conn, name); err != nil {
			return
		}
		host = string(name)
	case socksAddrIPv4:
		raw := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		host = net.IP(raw).String()
	case socksAddrIPv6:
		raw := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		host = net.IP(raw).String()
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return
	}

	f.mu.Lock()
	f.requested = append(f.requested, host)
	f.mu.Unlock()

	if f.opts.refuseWith != 0 {
		_, _ = conn.Write([]byte{socksVersion, f.opts.refuseWith, socksReserved,
			socksAddrIPv4, 0, 0, 0, 0, 0, 0})
		return
	}

	reply := []byte{socksVersion, socksSucceeded, socksReserved, f.opts.replyAddrType}
	switch f.opts.replyAddrType {
	case socksAddrIPv4:
		reply = append(reply, 0, 0, 0, 0)
	case socksAddrIPv6:
		reply = append(reply, make([]byte, net.IPv6len)...)
	case socksAddrDomain:
		reply = append(reply, 3, 'a', 'b', 'c')
	}
	reply = append(reply, 0, 0)
	if _, err := conn.Write(reply); err != nil {
		return
	}

	// Echo, so the caller can prove the connection is usable afterwards. This is
	// what catches a reply whose trailing address was not fully drained: the
	// leftover bytes would arrive here as if they were payload.
	_, _ = io.Copy(conn, conn)
}

// The reason this dialer is hand-rolled rather than taken from a library.
//
// **The hostname must reach the proxy unresolved.** Resolving it here and
// connecting to the resulting address would send a DNS query for the download
// host from the user's own network — to a resolver that is very often their
// internet provider's — which names the destination just as plainly as the
// connection would have, and defeats the entire point of proxying the transfer.
func TestTheProxyIsGivenTheHostnameRatherThanAnAddress(t *testing.T) {
	proxy := newFakeSocks(t, socksOpts{})
	dialer := socksDialer{Address: proxy.addr()}

	conn, err := dialer.DialContext(context.Background(), "tcp",
		"objects.githubusercontent.com:443")
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer func() { _ = conn.Close() }()

	asked := proxy.asked()
	if len(asked) != 1 {
		t.Fatalf("the proxy was asked for %d connections, want 1", len(asked))
	}
	if asked[0] != "objects.githubusercontent.com" {
		t.Errorf("the proxy was asked for %q; it must be the hostname, not an "+
			"address resolved on this machine", asked[0])
	}
}

// A literal address is passed as one, because there is no name to leak and the
// domain form would make the proxy resolve a string that is already an address.
func TestALiteralAddressIsSentAsAnAddress(t *testing.T) {
	proxy := newFakeSocks(t, socksOpts{})
	dialer := socksDialer{Address: proxy.addr()}

	for _, target := range []string{"192.0.2.10:443", "[2001:db8::1]:443"} {
		conn, err := dialer.DialContext(context.Background(), "tcp", target)
		if err != nil {
			t.Fatalf("DialContext(%s): %v", target, err)
		}
		_ = conn.Close()
	}

	asked := proxy.asked()
	if len(asked) != 2 {
		t.Fatalf("got %d requests, want 2", len(asked))
	}
	if asked[0] != "192.0.2.10" {
		t.Errorf("IPv4 target arrived as %q", asked[0])
	}
	if asked[1] != "2001:db8::1" {
		t.Errorf("IPv6 target arrived as %q", asked[1])
	}
}

// The reply's trailing address is variable-length and must be drained, or its
// leftover bytes are delivered to the caller as the first bytes of the response.
func TestTheReplyIsFullyDrainedForEveryAddressType(t *testing.T) {
	for name, kind := range map[string]byte{
		"ipv4":   socksAddrIPv4,
		"ipv6":   socksAddrIPv6,
		"domain": socksAddrDomain,
	} {
		t.Run(name, func(t *testing.T) {
			proxy := newFakeSocks(t, socksOpts{replyAddrType: kind})
			dialer := socksDialer{Address: proxy.addr()}

			conn, err := dialer.DialContext(context.Background(), "tcp", "example.invalid:443")
			if err != nil {
				t.Fatalf("DialContext: %v", err)
			}
			defer func() { _ = conn.Close() }()

			if _, err := conn.Write([]byte("hello")); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, 5)
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Fatalf("reading back: %v", err)
			}
			if string(got) != "hello" {
				t.Errorf("the first bytes off the connection were %q, not the echo — "+
					"part of the proxy's reply was left unread", got)
			}
		})
	}
}

func TestARefusalIsExplained(t *testing.T) {
	// 0x03, network unreachable: what Tor says while it is still bootstrapping,
	// which is the most likely refusal a user will ever see here.
	proxy := newFakeSocks(t, socksOpts{refuseWith: 0x03})
	dialer := socksDialer{Address: proxy.addr()}

	_, err := dialer.DialContext(context.Background(), "tcp", "example.invalid:443")
	if err == nil {
		t.Fatal("a refused connection was reported as successful")
	}
	if !strings.Contains(err.Error(), "still starting up") {
		t.Errorf("the error was %q, which does not explain what a Tor user should "+
			"conclude from it", err)
	}
}

func TestAProxyDemandingAuthenticationIsReportedClearly(t *testing.T) {
	proxy := newFakeSocks(t, socksOpts{demandAuth: true})
	dialer := socksDialer{Address: proxy.addr()}

	_, err := dialer.DialContext(context.Background(), "tcp", "example.invalid:443")
	if err == nil {
		t.Fatal("a proxy demanding authentication was accepted")
	}
	if !strings.Contains(err.Error(), "authentication") {
		t.Errorf("the error was %q", err)
	}
}

// A proxy that accepts and then says nothing must not hold the transfer open for
// ever. This is exactly how a Tor daemon behaves while it is still bootstrapping.
func TestASilentProxyDoesNotHangForEver(t *testing.T) {
	proxy := newFakeSocks(t, socksOpts{silent: true})
	dialer := socksDialer{Address: proxy.addr()}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := dialer.DialContext(ctx, "tcp", "example.invalid:443")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a silent proxy produced a usable connection")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the dial did not give up on a silent proxy")
	}
}

func TestAnUnreachableProxyIsNamedInTheError(t *testing.T) {
	// Port 1 on loopback: reliably refused, and never a real proxy.
	dialer := socksDialer{Address: "127.0.0.1:1"}
	_, err := dialer.DialContext(context.Background(), "tcp", "example.invalid:443")
	if err == nil {
		t.Fatal("dialing through a proxy that is not there succeeded")
	}
	if !strings.Contains(err.Error(), "cannot reach the proxy") {
		t.Errorf("the error was %q, which does not say the proxy was the problem", err)
	}
}

func TestNonTCPNetworksAreRefused(t *testing.T) {
	dialer := socksDialer{Address: "127.0.0.1:9050"}
	_, err := dialer.DialContext(context.Background(), "udp", "example.invalid:53")
	if err == nil {
		t.Error("a UDP dial through a SOCKS proxy was accepted")
	}
}

func TestUnusableTargetsAreRefusedBeforeDialing(t *testing.T) {
	dialer := socksDialer{Address: "127.0.0.1:9050"}
	for _, target := range []string{
		"no-port",
		"example.invalid:0",
		"example.invalid:70000",
		"example.invalid:notaport",
		strings.Repeat("a", 300) + ":443",
	} {
		if _, err := dialer.DialContext(context.Background(), "tcp", target); err == nil {
			t.Errorf("%q was accepted as a target", target)
		}
	}
}

func TestSocksFailureCoversEveryDefinedCode(t *testing.T) {
	for code := byte(1); code <= 8; code++ {
		if got := socksFailure(code); got == "" {
			t.Errorf("code %d has no explanation", code)
		}
	}
	if got := socksFailure(99); !strings.Contains(got, "99") {
		t.Errorf("an unknown code was described as %q, without naming it", got)
	}
}

// NewHTTPClient must never fall back to the environment's proxy settings.
//
// Go's transport otherwise consults HTTP_PROXY and friends, and a value there
// would silently take precedence over the address the user configured — sending
// a request meant for Tor somewhere else entirely.
func TestTheEnvironmentCannotRedirectTheDownload(t *testing.T) {
	for _, proxy := range []string{"", "127.0.0.1:9050"} {
		client := NewHTTPClient(proxy)
		transport, ok := client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("the client's transport is %T", client.Transport)
		}
		if transport.Proxy != nil {
			t.Errorf("with proxy=%q the transport still consults the environment "+
				"for a proxy", proxy)
		}
		if client.Timeout != 0 {
			t.Errorf("the client has an overall timeout of %s, which would abort a "+
				"healthy multi-hour transfer", client.Timeout)
		}
	}
}

// A redirect from https down to plain http is refused rather than followed.
func TestAnHTTPSToHTTPRedirectIsRefused(t *testing.T) {
	secure := mustRequest(t, "https://example.invalid/a")
	insecure := mustRequest(t, "http://example.invalid/b")

	if err := checkRedirect(insecure, []*http.Request{secure}); err == nil {
		t.Error("a redirect from https to http was followed")
	}
	if err := checkRedirect(secure, []*http.Request{secure}); err != nil {
		t.Errorf("an https to https redirect was refused: %v", err)
	}
}

func TestARedirectLoopIsBounded(t *testing.T) {
	req := mustRequest(t, "https://example.invalid/a")
	var via []*http.Request
	for i := 0; i < maxRedirects; i++ {
		via = append(via, req)
	}
	if err := checkRedirect(req, via); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("a chain of %d redirects was still being followed", len(via))
	}
}

func mustRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
