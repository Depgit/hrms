// Package mail sends the app's few emails (the joiner's link and code) from the company mailbox over SMTP, with
// the standard library only: net/smtp, STARTTLS (or TLS on port 465) and PLAIN authentication.
//
// Settings (backend/.env): SMTP_HOST, SMTP_PORT (587), SMTP_USER, SMTP_PASS, SMTP_FROM ("Saltstayz HR <hr@…>").
// Without SMTP_HOST nothing is sent: FromEnv returns nil and the app says email is not set up.
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Message is one email: a plain-text body and, optionally, the same content as simple HTML.
type Message struct {
	To      string // one address, "name@example.com"
	Subject string
	Text    string
	HTML    string
}

// Sender delivers messages. Kind is "smtp" for a real mailbox, "echo" for the development echo (written to the
// server log, never sent), anything else for a test double.
type Sender interface {
	Send(ctx context.Context, m Message) error
	Kind() string
}

// SMTP sends through a mail server with STARTTLS (port 587, or any port whose server offers it) or implicit TLS
// (port 465), and PLAIN authentication when a user is set. It never authenticates over a connection that is not
// encrypted, except to a server on this machine (Go's net/smtp rule).
type SMTP struct {
	Host string
	Port int
	User string
	Pass string
	From string // "Saltstayz HR <hr@example.com>" or a bare address
	// Timeout for the whole conversation (default 30 seconds).
	Timeout time.Duration
}

// FromEnv builds the SMTP sender from SMTP_HOST, SMTP_PORT, SMTP_USER, SMTP_PASS and SMTP_FROM; nil, nil when
// SMTP_HOST is empty (email not set up).
func FromEnv(getenv func(string) string) (*SMTP, error) {
	host := strings.TrimSpace(getenv("SMTP_HOST"))
	if host == "" {
		return nil, nil
	}
	port := 587
	if p := strings.TrimSpace(getenv("SMTP_PORT")); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("SMTP_PORT %q is not a port number", p)
		}
		port = n
	}
	s := &SMTP{Host: host, Port: port, User: strings.TrimSpace(getenv("SMTP_USER")), Pass: getenv("SMTP_PASS"), From: strings.TrimSpace(getenv("SMTP_FROM"))}
	if s.From == "" {
		s.From = s.User
	}
	if _, err := mail.ParseAddress(s.From); err != nil {
		return nil, fmt.Errorf("SMTP_FROM %q is not an email address (for example: Saltstayz HR <hr@example.com>)", s.From)
	}
	return s, nil
}

func (s *SMTP) Kind() string { return "smtp" }

// Send delivers one message.
func (s *SMTP) Send(ctx context.Context, m Message) error {
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return fmt.Errorf("the sender address is not valid: %w", err)
	}
	to, err := mail.ParseAddress(m.To)
	if err != nil {
		return fmt.Errorf("the address %q is not valid", m.To)
	}
	raw, err := Build(s.From, m, time.Now())
	if err != nil {
		return err
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	d := net.Dialer{Timeout: timeout}
	var conn net.Conn
	if s.Port == 465 {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("could not reach the mail server %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mail server %s: %w", addr, err)
	}
	defer c.Close()
	if err := c.Hello(helloName()); err != nil {
		return err
	}
	if ok, _ := c.Extension("STARTTLS"); ok && s.Port != 465 {
		if err := c.StartTLS(&tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("mail server %s: STARTTLS failed: %w", addr, err)
		}
	}
	if s.User != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			return fmt.Errorf("mail server %s does not offer sign-in (AUTH)", addr)
		}
		if err := c.Auth(smtp.PlainAuth("", s.User, s.Pass, s.Host)); err != nil {
			return fmt.Errorf("mail server sign-in failed: %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(to.Address); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func helloName() string {
	return "localhost"
}

// Build renders a message as RFC 5322 text: headers (encoded when not plain ASCII) and a multipart/alternative
// body in quoted-printable. Line breaks in To or Subject are refused (header injection).
func Build(from string, m Message, now time.Time) ([]byte, error) {
	for _, v := range []string{from, m.To, m.Subject} {
		if strings.ContainsAny(v, "\r\n") {
			return nil, errors.New("line breaks are not allowed in email headers")
		}
	}
	fa, err := mail.ParseAddress(from)
	if err != nil {
		return nil, fmt.Errorf("the sender address is not valid: %w", err)
	}
	ta, err := mail.ParseAddress(m.To)
	if err != nil {
		return nil, fmt.Errorf("the address %q is not valid", m.To)
	}
	var b bytes.Buffer
	domain := "localhost"
	if i := strings.LastIndex(fa.Address, "@"); i >= 0 {
		domain = fa.Address[i+1:]
	}
	h := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	h("From", fa.String())
	h("To", ta.String())
	h("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h("Date", now.Format(time.RFC1123Z))
	h("Message-ID", "<"+randHex(12)+"@"+domain+">")
	h("MIME-Version", "1.0")
	part := func(ctype, body string) {
		fmt.Fprintf(&b, "Content-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", ctype)
		w := quotedprintable.NewWriter(&b)
		_, _ = w.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")))
		_ = w.Close()
		b.WriteString("\r\n")
	}
	if m.HTML == "" {
		part("text/plain", m.Text)
		return b.Bytes(), nil
	}
	boundary := "hrms-" + randHex(12)
	h("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")
	b.WriteString("--" + boundary + "\r\n")
	part("text/plain", m.Text)
	b.WriteString("--" + boundary + "\r\n")
	part("text/html", m.HTML)
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes(), nil
}

func randHex(n int) string {
	p := make([]byte, n)
	_, _ = rand.Read(p)
	return hex.EncodeToString(p)
}

// Echo is the development stand-in (OTP_DEV_ECHO=true without SMTP): it writes each message to the server log and
// sends nothing.
type Echo struct{}

func (Echo) Kind() string { return "echo" }

func (Echo) Send(_ context.Context, m Message) error {
	log.Printf("DEV MAIL (not sent) to %s: %s\n%s", m.To, m.Subject, m.Text)
	return nil
}

// Fake records messages instead of sending them (tests).
type Fake struct {
	mu   sync.Mutex
	Sent []Message
	Err  error // returned by Send when set
}

func (f *Fake) Kind() string { return "smtp" }

func (f *Fake) Send(_ context.Context, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.Sent = append(f.Sent, m)
	return nil
}

// Last is the newest message sent to the address ("" for any), or a zero Message.
func (f *Fake) Last(to string) Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.Sent) - 1; i >= 0; i-- {
		if to == "" || f.Sent[i].To == to {
			return f.Sent[i]
		}
	}
	return Message{}
}

// Count is how many messages were sent.
func (f *Fake) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Sent)
}
