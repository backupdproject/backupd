// Package emailtest offers an in-process SMTP sink for tests that have to
// drive a real enrollment.
//
// It exists because of what issue #830 did to every automated caller of
// POST /api/v1/auth/enroll. That route now sends a confirmation message
// and refuses the enrollment if the send fails, so a harness that enrolls
// an administrator in order to get a session - the performance baseline,
// the CLI/API equivalence suite, the web host's own engine tests - needs
// something on the other end of an SMTP connection. A mail sink CONTAINER
// is the right answer where the runtime under test is itself in a
// container (apps/generic/tests/dockercli), and the wrong one for a
// harness running an engine as a child process on this same host: it adds
// a docker dependency, an image pull and seconds of startup to a test
// whose subject is not email at all.
//
// So this is the host-side answer: a listener on 127.0.0.1 that speaks
// enough SMTP to accept one message, started and stopped by the test that
// needs it. It stores what it received in memory, so a harness can also
// ASSERT on the confirmation rather than merely allowing it to succeed.
//
// It deliberately does NOT speak STARTTLS or implicit TLS, and callers
// point at it with security "none". apps/common/email's own tests cover
// the encrypted modes against a TLS server, and a sink that offered a
// self-signed certificate would make every caller here configure trust
// for it.
package emailtest

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Sink is a running in-process SMTP server.
type Sink struct {
	listener net.Listener

	mu       sync.Mutex
	messages []Message

	wg   sync.WaitGroup
	done chan struct{}
}

// Message is one message the sink accepted: the envelope addresses it was
// given and the raw DATA payload, headers included.
type Message struct {
	From string
	To   []string
	Data string
}

// Subject reads the Subject header out of Data, or "" if there is none.
// Provided because "did the confirmation arrive" is the question every
// caller actually has, and none of them should be parsing headers for it.
func (m Message) Subject() string {
	for _, line := range strings.Split(m.Data, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			return ""
		}
		if rest, ok := cutPrefixFold(line, "subject:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

// Start runs a sink on 127.0.0.1 and stops it when t finishes - including
// when t fails, which is the case a manual Close in the test body would
// miss.
func Start(t *testing.T) *Sink {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("emailtest: listen: %v", err)
	}
	s := &Sink{listener: listener, done: make(chan struct{})}
	go s.serve()
	t.Cleanup(s.Close)
	return s
}

// Close stops accepting and waits for in-flight sessions to finish. Safe
// to call more than once.
func (s *Sink) Close() {
	select {
	case <-s.done:
		return
	default:
	}
	close(s.done)
	_ = s.listener.Close()
	s.wg.Wait()
}

// Host and Port are what a caller puts in an SMTP configuration.
func (s *Sink) Host() string {
	host, _, _ := net.SplitHostPort(s.listener.Addr().String())
	return host
}

func (s *Sink) Port() int {
	_, port, _ := net.SplitHostPort(s.listener.Addr().String())
	n, _ := strconv.Atoi(port)
	return n
}

// Addr is Host:Port.
func (s *Sink) Addr() string { return s.listener.Addr().String() }

// Messages returns everything received so far.
func (s *Sink) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.messages...)
}

// WaitForMessage waits up to timeout for a message whose Subject is
// subject, and returns it.
//
// Waiting rather than reading once: the caller's own HTTP request has
// returned by the time it asks, but the send happened on another
// goroutine in the process under test in at least one case
// (forgot-password), and "not yet" is not "never".
func (s *Sink) WaitForMessage(t *testing.T, subject string, timeout time.Duration) Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, m := range s.Messages() {
			if m.Subject() == subject {
				return m
			}
		}
		if time.Now().After(deadline) {
			var got []string
			for _, m := range s.Messages() {
				got = append(got, m.Subject())
			}
			t.Fatalf("emailtest: no message with subject %q arrived within %s; received %v", subject, timeout, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (s *Sink) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.session(conn)
		}()
	}
}

// session speaks the subset of RFC 5321 net/smtp needs to hand over one
// message: a greeting, EHLO/HELO, MAIL FROM, RCPT TO, DATA and QUIT.
// Anything else gets a 500, which is what a real server would say.
func (s *Sink) session(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))

	reader := bufio.NewReader(conn)
	write := func(format string, args ...any) bool {
		_, err := fmt.Fprintf(conn, format+"\r\n", args...)
		return err == nil
	}
	if !write("220 emailtest ESMTP ready") {
		return
	}

	var from string
	var to []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(verb, "EHLO"):
			write("250-emailtest")
			write("250 AUTH PLAIN")
		case strings.HasPrefix(verb, "HELO"):
			write("250 emailtest")
		case strings.HasPrefix(verb, "AUTH"):
			// Accepted without checking: this sink authenticates nobody
			// and stores no credential. A caller that sends one is
			// exercising its own configuration path, not ours.
			write("235 2.7.0 authentication succeeded")
		case strings.HasPrefix(verb, "MAIL FROM"):
			from = addressIn(line)
			write("250 2.1.0 ok")
		case strings.HasPrefix(verb, "RCPT TO"):
			to = append(to, addressIn(line))
			write("250 2.1.5 ok")
		case verb == "DATA":
			write("354 end with <CRLF>.<CRLF>")
			var data strings.Builder
			for {
				dataLine, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if dataLine == ".\r\n" || dataLine == ".\n" {
					break
				}
				// Dot-unstuffing, the counterpart to what net/smtp's
				// DataWriter does on the way out.
				data.WriteString(strings.TrimPrefix(dataLine, ".."))
			}
			s.mu.Lock()
			s.messages = append(s.messages, Message{From: from, To: to, Data: data.String()})
			s.mu.Unlock()
			from, to = "", nil
			write("250 2.0.0 queued")
		case verb == "RSET":
			from, to = "", nil
			write("250 2.0.0 ok")
		case verb == "QUIT":
			write("221 2.0.0 bye")
			return
		default:
			write("500 5.5.2 unrecognised command")
		}
	}
}

// addressIn reads the address out of "MAIL FROM:<a@b>" / "RCPT TO:<a@b>".
func addressIn(line string) string {
	_, rest, ok := strings.Cut(line, ":")
	if !ok {
		return ""
	}
	rest = strings.TrimSpace(rest)
	rest = strings.TrimPrefix(rest, "<")
	if i := strings.Index(rest, ">"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}
