package alert

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is the smallest server Go's smtp client will talk to. Enough to prove
// a real message crosses a real socket in the right order, which a mock of the
// client would not.
type fakeSMTP struct {
	ln net.Listener

	mu       sync.Mutex
	messages []string
	envelope []string

	rejectRcpt bool
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *fakeSMTP) hostPort(t *testing.T) (host string, port int) {
	t.Helper()
	h, portStr, err := net.SplitHostPort(s.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return h, p
}

func (s *fakeSMTP) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)

	say := func(line string) {
		_, _ = w.WriteString(line + "\r\n")
		_ = w.Flush()
	}
	say("220 fake ESMTP")

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))

		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250 fake")
		case strings.HasPrefix(cmd, "MAIL FROM"), strings.HasPrefix(cmd, "RCPT TO"):
			if s.rejectRcpt && strings.HasPrefix(cmd, "RCPT TO") {
				say("550 no such user")
				continue
			}
			s.mu.Lock()
			s.envelope = append(s.envelope, strings.TrimSpace(line))
			s.mu.Unlock()
			say("250 ok")
		case cmd == "DATA":
			say("354 send it")
			var body strings.Builder
			for {
				dataLine, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dataLine, "\r\n") == "." {
					break
				}
				body.WriteString(dataLine)
			}
			s.mu.Lock()
			s.messages = append(s.messages, body.String())
			s.mu.Unlock()
			say("250 queued")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (s *fakeSMTP) received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.messages...)
}

func fixedClock() func() time.Time {
	return func() time.Time { return time.Unix(1_790_000_000, 0).UTC() }
}

func TestSMTPDeliversARealMessage(t *testing.T) {
	t.Parallel()

	srv := newFakeSMTP(t)
	host, port := srv.hostPort(t)

	m, err := NewSMTP(SMTPOptions{
		Name: "email", Host: host, Port: port,
		From: "forktower@example.com", To: "me@example.com, you@example.com",
		Timeout: 5 * time.Second, Now: fixedClock(),
	})
	if err != nil {
		t.Fatal(err)
	}

	err = m.Send(context.Background(), Payload{
		Tier: "critical", Kind: KindViewWrongBranch, Message: "Open Forktower.",
	})
	if err != nil {
		t.Fatalf("sending failed: %v", err)
	}

	got := srv.received()
	if len(got) != 1 {
		t.Fatalf("server received %d messages, want 1", len(got))
	}
	msg := got[0]

	for _, want := range []string{
		"From: forktower@example.com",
		"To: me@example.com, you@example.com",
		"Subject: " + headlineUrgent,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"X-Forktower-Kind: " + KindViewWrongBranch,
		"Open Forktower.",
		"Date: ",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message is missing %q:\n%s", want, msg)
		}
	}
	// Headers and body must be separated by a blank line, or the whole thing is
	// read as headers and the user sees an empty email.
	if !strings.Contains(msg, "\r\n\r\n") {
		t.Errorf("no header/body separator:\n%q", msg)
	}
}

// A subject carrying raw UTF-8 is mangled by some servers, which turns an alert
// into gibberish exactly when it is being read in a hurry.
func TestSMTPEncodesItsSubject(t *testing.T) {
	t.Parallel()

	m, err := NewSMTP(SMTPOptions{
		Name: "email", Host: loopbackName, Port: 25,
		From: "a@example.com", To: "b@example.com", Now: fixedClock(),
	})
	if err != nil {
		t.Fatal(err)
	}

	msg := string(m.message(Payload{Tier: "info", Message: "chains — separated"}))
	for _, line := range strings.Split(msg, "\r\n") {
		if line == "" {
			break // headers end
		}
		for _, r := range line {
			if r > 0x7F {
				t.Errorf("header line %q carries a non-ASCII rune %q", line, r)
			}
		}
	}
	// The body is where UTF-8 belongs, and it must survive intact.
	if !strings.Contains(msg, "chains — separated") {
		t.Errorf("the body was mangled:\n%s", msg)
	}
}

func TestSMTPSubjectFollowsTheTier(t *testing.T) {
	t.Parallel()

	m, err := NewSMTP(SMTPOptions{
		Name: "email", Host: loopbackName, Port: 25,
		From: "a@example.com", To: "b@example.com", Now: fixedClock(),
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"critical": headlineUrgent,
		"loss":     headlineUrgent,
		"warning":  headlineAttention,
		"info":     headlineRoutine,
		"resolved": headlineRoutine,
	}
	for tier, want := range cases {
		got := smtpSubject(Payload{Tier: tier})
		if !strings.Contains(got, want) {
			t.Errorf("tier %q gave subject %q, want it to contain %q", tier, got, want)
		}
		// Never a bare severity word: a subject line has to say what to do.
		if got == tier {
			t.Errorf("the subject for %q is a bare tier name", tier)
		}
	}
	_ = m
}

// Sending a password over an unencrypted connection is a real leak, and finding
// out at the moment an alert was needed is the worst time.
func TestNewSMTPRefusesToSendCredentialsInTheClear(t *testing.T) {
	t.Parallel()

	base := SMTPOptions{
		Name: "email", Host: "mail.example.com", Port: 587,
		From: "a@example.com", To: "b@example.com",
	}

	withPassword := base
	withPassword.User = "me"
	withPassword.Pass = "hunter2"
	if _, err := NewSMTP(withPassword); err == nil {
		t.Error("accepted a login over an unencrypted connection")
	}

	secured := withPassword
	secured.StartTLS = true
	if _, err := NewSMTP(secured); err != nil {
		t.Errorf("refused a properly secured login: %v", err)
	}

	// A server on this machine is a different matter: nothing crosses a network.
	local := withPassword
	local.Host = "127.0.0.1"
	if _, err := NewSMTP(local); err != nil {
		t.Errorf("refused a login to a server on this machine: %v", err)
	}
}

func TestNewSMTPRejectsWhatCouldNotSend(t *testing.T) {
	t.Parallel()

	ok := SMTPOptions{
		Name: "email", Host: "mail.example.com", Port: 587,
		From: "a@example.com", To: "b@example.com",
	}

	cases := map[string]func(*SMTPOptions){
		"no name":            func(o *SMTPOptions) { o.Name = "" },
		"no host":            func(o *SMTPOptions) { o.Host = "" },
		"port zero":          func(o *SMTPOptions) { o.Port = 0 },
		"port out of range":  func(o *SMTPOptions) { o.Port = 70000 },
		"no sender":          func(o *SMTPOptions) { o.From = "" },
		"unusable sender":    func(o *SMTPOptions) { o.From = "not an address" },
		"no recipients":      func(o *SMTPOptions) { o.To = "" },
		"only commas":        func(o *SMTPOptions) { o.To = " , , " },
		"unusable recipient": func(o *SMTPOptions) { o.To = "me@example.com, nonsense" },
	}

	for name, break_ := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o := ok
			break_(&o)
			if _, err := NewSMTP(o); err == nil {
				t.Error("accepted a configuration that could never send")
			}
		})
	}
}

func TestSMTPReportsARejectedRecipient(t *testing.T) {
	t.Parallel()

	srv := newFakeSMTP(t)
	srv.rejectRcpt = true
	host, port := srv.hostPort(t)

	m, err := NewSMTP(SMTPOptions{
		Name: "email", Host: host, Port: port,
		From: "a@example.com", To: "b@example.com",
		Timeout: 5 * time.Second, Now: fixedClock(),
	})
	if err != nil {
		t.Fatal(err)
	}

	err = m.Send(context.Background(), Payload{Tier: "info"})
	if err == nil {
		t.Fatal("a rejected recipient was reported as delivered")
	}
	if !strings.Contains(err.Error(), "recipient") {
		t.Errorf("the error does not say what was rejected: %v", err)
	}
}

// A mail server that accepts the connection and then says nothing must not hold
// up the transports behind it.
func TestSMTPHonoursItsContext(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	m, err := NewSMTP(SMTPOptions{
		Name: "email", Host: host, Port: port,
		From: "a@example.com", To: "b@example.com",
		Timeout: 100 * time.Millisecond, Now: fixedClock(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Nothing accepts, so the dial itself has to time out.
	_ = ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	if err := m.Send(ctx, Payload{Tier: "info"}); err == nil {
		t.Fatal("sending to nothing was reported as delivered")
	} else if errors.Is(err, context.DeadlineExceeded) && time.Since(start) > 5*time.Second {
		t.Errorf("Send waited %v", time.Since(start))
	}
}
