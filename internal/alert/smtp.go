package alert

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/paulscode/forktower/internal/store"
)

// SMTP delivers by email.
//
// Email through someone else's server is a third-party transport like any other:
// the operator sees every subject line. A self-hosted server is the documented
// private option, and the content-free default keeps the rest local either way.
type SMTP struct {
	name     string
	host     string
	port     int
	user     string
	pass     string
	from     string
	to       []string
	startTLS bool

	dialer *net.Dialer
	// now is injected so a test can assert on a whole message including its date.
	now func() time.Time
}

var _ Transport = (*SMTP)(nil)

// SMTPOptions is what an SMTP transport needs. Grouped into a struct because
// seven positional parameters is a swap waiting to happen, and swapping `user`
// with `from` produces a transport that fails only when it is finally needed.
type SMTPOptions struct {
	Name     string
	Host     string
	Port     int
	User     string
	Pass     string
	From     string
	To       string
	StartTLS bool
	Timeout  time.Duration
	// Now is optional; nil reads the real clock.
	Now func() time.Time
}

// NewSMTP builds an email transport, rejecting anything that could not send.
//
// Addresses are parsed here rather than at the first alert. A malformed address
// that only surfaces during a chain split has cost the user the one thing this
// software is for.
func NewSMTP(o SMTPOptions) (*SMTP, error) {
	if o.Name == "" {
		return nil, errors.New("smtp transport needs a name")
	}
	if o.Host == "" {
		return nil, fmt.Errorf("smtp transport %q needs a host", o.Name)
	}
	if o.Port <= 0 || o.Port > 65535 {
		return nil, fmt.Errorf("smtp transport %q has port %d, which is not a port",
			o.Name, o.Port)
	}
	if _, err := mail.ParseAddress(o.From); err != nil {
		return nil, fmt.Errorf("smtp transport %q has an unusable from address %q: %w",
			o.Name, o.From, err)
	}
	to, err := parseRecipients(o.To)
	if err != nil {
		return nil, fmt.Errorf("smtp transport %q: %w", o.Name, err)
	}
	// Credentials with no encryption would be sent in the clear. Go's own PlainAuth
	// refuses that too, but failing here says so at startup instead of at the
	// moment an alert was needed.
	if o.User != "" && !o.StartTLS && !isLoopback(o.Host) {
		return nil, fmt.Errorf(
			"smtp transport %q would send its password unencrypted; set starttls = true",
			o.Name)
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultSendTimeout
	}
	if o.Now == nil {
		o.Now = time.Now
	}

	return &SMTP{
		name: o.Name, host: o.Host, port: o.Port,
		user: o.User, pass: o.Pass, from: o.From, to: to, startTLS: o.StartTLS,
		dialer: &net.Dialer{Timeout: o.Timeout},
		now:    o.Now,
	}, nil
}

func parseRecipients(raw string) ([]string, error) {
	fields := strings.Split(raw, ",")
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		addr, err := mail.ParseAddress(f)
		if err != nil {
			return nil, fmt.Errorf("unusable recipient %q: %w", f, err)
		}
		out = append(out, addr.Address)
	}
	if len(out) == 0 {
		return nil, errors.New("needs at least one recipient address")
	}
	return out, nil
}

// loopbackName is this machine by name. A connection to it never crosses a
// network, which is the one case where sending a password without encryption is
// not a leak.
const loopbackName = "localhost"

func isLoopback(host string) bool {
	if host == loopbackName {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Name implements Transport.
func (s *SMTP) Name() string { return s.name }

// Send implements Transport.
//
// Dialled through a context-aware dialer rather than smtp.Dial, so a server that
// accepts the connection and then says nothing cannot hold up the transports
// behind it.
func (s *SMTP) Send(ctx context.Context, p Payload) error {
	addr := net.JoinHostPort(s.host, strconv.Itoa(s.port))

	conn, err := s.dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("connecting to the mail server: %w", err)
	}

	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("starting the mail session: %w", err)
	}
	// Quit closes the connection on the happy path; Close is the backstop for
	// every path that returns early.
	defer func() { _ = c.Close() }()

	if s.startTLS {
		if err := c.StartTLS(&tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("securing the mail session: %w", err)
		}
	}
	if s.user != "" {
		if err := c.Auth(smtp.PlainAuth("", s.user, s.pass, s.host)); err != nil {
			return fmt.Errorf("the mail server rejected the login: %w", err)
		}
	}

	if err := c.Mail(s.from); err != nil {
		return fmt.Errorf("the mail server rejected the sender: %w", err)
	}
	for _, rcpt := range s.to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("the mail server rejected a recipient: %w", err)
		}
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("the mail server refused the message: %w", err)
	}
	if _, err := w.Write(s.message(p)); err != nil {
		return fmt.Errorf("writing the message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("the mail server rejected the message: %w", err)
	}
	return c.Quit()
}

// message renders one alert as an email.
//
// The subject is encoded rather than assumed to be ASCII: a header carrying raw
// UTF-8 is silently mangled by some servers, which would turn an alert into
// gibberish exactly when it is being read in a hurry.
func (s *SMTP) message(p Payload) []byte {
	var b strings.Builder
	b.WriteString("From: " + s.from + "\r\n")
	b.WriteString("To: " + strings.Join(s.to, ", ") + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", smtpSubject(p)) + "\r\n")
	b.WriteString("Date: " + s.now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	// A stable header so a user's own mail rules can sort on the category without
	// parsing the subject line.
	b.WriteString("X-Forktower-Kind: " + p.Kind + "\r\n")
	b.WriteString("\r\n")
	b.WriteString(p.Message + "\r\n")
	return []byte(b.String())
}

// smtpSubject is the line a user sees in their inbox.
//
// Plain ASCII by construction, so it survives the encoder above untouched. An
// encoded subject is decoded correctly by every modern client, but it is
// unreadable in server logs and in the handful of places that get it wrong — and
// a subject line is exactly what someone squints at in a hurry.
func smtpSubject(p Payload) string {
	return Headline(store.Tier(p.Tier))
}
