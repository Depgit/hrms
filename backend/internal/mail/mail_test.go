package mail

import (
	"bufio"
	"context"
	"encoding/base64"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if s, err := FromEnv(env(nil)); s != nil || err != nil {
		t.Fatalf("no host: %v %v", s, err)
	}
	s, err := FromEnv(env(map[string]string{"SMTP_HOST": "smtp.example.com", "SMTP_USER": "hr@example.com", "SMTP_PASS": "x", "SMTP_FROM": "Saltstayz HR <hr@example.com>"}))
	if err != nil || s.Port != 587 || s.Host != "smtp.example.com" {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	if _, err := FromEnv(env(map[string]string{"SMTP_HOST": "h", "SMTP_PORT": "abc"})); err == nil {
		t.Fatal("bad port")
	}
	if _, err := FromEnv(env(map[string]string{"SMTP_HOST": "h", "SMTP_FROM": "not an address"})); err == nil {
		t.Fatal("bad from")
	}
}

func TestBuild(t *testing.T) {
	raw, err := Build("Saltstayz HR <hr@example.com>", Message{To: "meera@example.com", Subject: "Complete your joining details — Saltstayz", Text: "Hello\nLink: https://x/join/abc", HTML: "<p>Hello</p>"}, time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{"From: \"Saltstayz HR\" <hr@example.com>\r\n", "To: <meera@example.com>\r\n", "Subject: =?utf-8?q?", "multipart/alternative", "text/plain; charset=utf-8", "text/html; charset=utf-8", "Link: https://x/join/abc"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in\n%s", want, s)
		}
	}
	if _, err := Build("hr@example.com", Message{To: "a@example.com\r\nBcc: x@evil.com", Subject: "x"}, time.Now()); err == nil {
		t.Fatal("header injection accepted")
	}
	if _, err := Build("hr@example.com", Message{To: "a@example.com", Subject: "x\nBcc: y@evil.com"}, time.Now()); err == nil {
		t.Fatal("header injection in subject accepted")
	}
}

// A tiny SMTP server on this machine (no TLS: net/smtp allows PLAIN sign-in to localhost only) records one message.
func TestSendOverSMTP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan map[string]string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r, w := bufio.NewReader(c), bufio.NewWriter(c)
		out := map[string]string{}
		say := func(s string) { w.WriteString(s + "\r\n"); w.Flush() }
		say("220 test ESMTP")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				got <- out
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "EHLO"):
				say("250-test")
				say("250 AUTH PLAIN")
			case strings.HasPrefix(line, "AUTH PLAIN"):
				b, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "AUTH PLAIN "))
				out["auth"] = strings.ReplaceAll(string(b), "\x00", "|")
				say("235 ok")
			case strings.HasPrefix(line, "MAIL FROM:"):
				out["from"] = line
				say("250 ok")
			case strings.HasPrefix(line, "RCPT TO:"):
				out["to"] = line
				say("250 ok")
			case line == "DATA":
				say("354 go")
				var body strings.Builder
				for {
					l, _ := r.ReadString('\n')
					if l == ".\r\n" {
						break
					}
					body.WriteString(l)
				}
				out["data"] = body.String()
				say("250 queued")
			case line == "QUIT":
				say("221 bye")
				got <- out
				return
			default:
				say("250 ok")
			}
		}
	}()
	port, _ := strconv.Atoi(strings.Split(ln.Addr().String(), ":")[1])
	s := &SMTP{Host: "127.0.0.1", Port: port, User: "hr@example.com", Pass: "secret", From: "Saltstayz HR <hr@example.com>", Timeout: 5 * time.Second}
	if err := s.Send(context.Background(), Message{To: "meera@example.com", Subject: "Your code", Text: "Your code is 123456."}); err != nil {
		t.Fatal(err)
	}
	m := <-got
	if m["auth"] != "|hr@example.com|secret" || m["from"] != "MAIL FROM:<hr@example.com>" || m["to"] != "RCPT TO:<meera@example.com>" || !strings.Contains(m["data"], "Your code is 123456.") {
		t.Fatalf("server saw %v", m)
	}
}
