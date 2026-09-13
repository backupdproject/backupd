package email

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every test here runs against an SMTP server this file starts inside the
// test process. That is not a stand-in for the container test in
// apps/common/auth/local (which proves a real mail sink receives what this
// package sends): it is how the two ENCRYPTED modes get covered at all,
// since the ephemeral sinks are plaintext-only, and it is how a failure
// mid-conversation becomes something a test can ask for on demand.

// fakeSMTP is a submission server that speaks just enough of RFC 5321 for
// net/smtp to complete a session against it, plus the two knobs the tests
// need: a stage at which to refuse, and whether to offer STARTTLS.
type fakeSMTP struct {
	t         *testing.T
	listener  net.Listener
	tlsConfig *tls.Config

	// implicitTLS wraps the connection before the greeting (port-465
	// style) rather than offering STARTTLS.
	implicitTLS bool
	// offerStartTLS advertises STARTTLS in the EHLO response.
	offerStartTLS bool
	// refuseAt is the verb this server answers with a permanent error, or
	// "" to accept everything. "AUTH", "MAIL", "RCPT" and "DATA-END" are
	// the ones the tests use.
	refuseAt string

	mu       sync.Mutex
	received []string
	authSeen []string
	tlsUsed  bool
	wg       sync.WaitGroup
}

func (s *fakeSMTP) addr() string { return s.listener.Addr().String() }

func (s *fakeSMTP) port() int {
	_, p, err := net.SplitHostPort(s.addr())
	if err != nil {
		s.t.Fatalf("SplitHostPort(%q): %v", s.addr(), err)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		s.t.Fatalf("Atoi(%q): %v", p, err)
	}
	return n
}

func (s *fakeSMTP) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.received...)
}

func (s *fakeSMTP) credentials() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.authSeen...)
}

func (s *fakeSMTP) sawTLS() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tlsUsed
}

// start listens on a loopback port and serves connections until the test
// finishes. The listener is closed and every connection goroutine joined
// by t.Cleanup, including on a failing test, so no goroutine and no socket
// outlives the test that created it.
func (s *fakeSMTP) start() {
	s.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.t.Fatalf("listen: %v", err)
	}
	s.listener = ln
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.serve(conn)
			}()
		}
	}()
	s.t.Cleanup(func() {
		_ = ln.Close()
		<-done
		s.wg.Wait()
	})
}

func (s *fakeSMTP) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	if s.implicitTLS {
		tlsConn := tls.Server(conn, s.tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		s.mu.Lock()
		s.tlsUsed = true
		s.mu.Unlock()
		conn = tlsConn
	}

	r := bufio.NewReader(conn)
	write := func(line string) bool {
		_, err := conn.Write([]byte(line + "\r\n"))
		return err == nil
	}
	if !write("220 fake.smtp.test ESMTP ready") {
		return
	}

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(verb, "EHLO"):
			write("250-fake.smtp.test")
			if s.offerStartTLS {
				write("250-STARTTLS")
			}
			write("250 AUTH PLAIN")
		case strings.HasPrefix(verb, "HELO"):
			write("250 fake.smtp.test")
		case verb == "STARTTLS":
			write("220 ready to start TLS")
			tlsConn := tls.Server(conn, s.tlsConfig)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			s.mu.Lock()
			s.tlsUsed = true
			s.mu.Unlock()
			conn = tlsConn
			r = bufio.NewReader(conn)
		case strings.HasPrefix(verb, "AUTH PLAIN"):
			if s.refuseAt == "AUTH" {
				write("535 5.7.8 authentication credentials invalid")
				continue
			}
			fields := strings.Fields(line)
			if len(fields) == 3 {
				if raw, err := base64.StdEncoding.DecodeString(fields[2]); err == nil {
					s.mu.Lock()
					s.authSeen = append(s.authSeen, string(raw))
					s.mu.Unlock()
				}
			}
			write("235 2.7.0 authentication succeeded")
		case strings.HasPrefix(verb, "MAIL FROM"):
			if s.refuseAt == "MAIL" {
				write("550 5.1.8 sender rejected")
				continue
			}
			write("250 2.1.0 ok")
		case strings.HasPrefix(verb, "RCPT TO"):
			if s.refuseAt == "RCPT" {
				write("550 5.1.1 no such recipient")
				continue
			}
			write("250 2.1.5 ok")
		case verb == "DATA":
			if s.refuseAt == "DATA" {
				write("451 4.3.0 try later")
				continue
			}
			write("354 end with <CRLF>.<CRLF>")
			var body strings.Builder
			for {
				dataLine, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if dataLine == ".\r\n" || dataLine == ".\n" {
					break
				}
				body.WriteString(dataLine)
			}
			if s.refuseAt == "DATA-END" {
				write("552 5.3.4 message too large")
				continue
			}
			s.mu.Lock()
			s.received = append(s.received, body.String())
			s.mu.Unlock()
			write("250 2.0.0 queued")
		case verb == "QUIT":
			write("221 2.0.0 bye")
			return
		case verb == "RSET" || strings.HasPrefix(verb, "NOOP"):
			write("250 2.0.0 ok")
		default:
			write("500 5.5.2 unrecognised command")
		}
	}
}

// selfSignedTLS returns a server TLS configuration for "127.0.0.1" and
// installs the certificate as the only root tlsConfigFor trusts for the
// duration of the test. Replacing tlsConfigFor is what lets the encrypted
// modes be exercised at all without a real certificate authority; it is
// restored afterwards so no other test in this package inherits it.
func selfSignedTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	previous := tlsConfigFor
	tlsConfigFor = func(host string) *tls.Config {
		return &tls.Config{ServerName: host, RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	t.Cleanup(func() { tlsConfigFor = previous })

	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}},
		MinVersion:   tls.VersionTLS12,
	}
}

func newFake(t *testing.T, configure func(*fakeSMTP)) *fakeSMTP {
	t.Helper()
	s := &fakeSMTP{t: t}
	configure(s)
	s.start()
	return s
}

func configFor(s *fakeSMTP, security Security) Config {
	return Config{
		Host:     "127.0.0.1",
		Port:     s.port(),
		Security: security,
		From:     "backupd@example.com",
	}
}

func TestSend_PlaintextSessionDeliversTheComposedMessage(t *testing.T) {
	server := newFake(t, func(s *fakeSMTP) {})

	err := Send(context.Background(), configFor(server, SecurityNone), Message{
		To:      "admin@example.com",
		Subject: "backupd: recovery email confirmed",
		Body:    "line one\nline two",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	msgs := server.messages()
	if len(msgs) != 1 {
		t.Fatalf("the server received %d messages, want exactly 1", len(msgs))
	}
	got := msgs[0]
	for _, want := range []string{
		"From: backupd@example.com\r\n",
		"To: admin@example.com\r\n",
		"Subject: backupd: recovery email confirmed\r\n",
		"Content-Type: text/plain; charset=utf-8\r\n",
		"\r\nline one\r\nline two\r\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the delivered message does not contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\n\n") {
		t.Errorf("the delivered message contains a bare LF pair, so its headers are not CRLF-terminated:\n%q", got)
	}
}

func TestSend_StartTLSUpgradesBeforeAuthenticating(t *testing.T) {
	tlsConfig := selfSignedTLS(t)
	server := newFake(t, func(s *fakeSMTP) {
		s.tlsConfig = tlsConfig
		s.offerStartTLS = true
	})

	cfg := configFor(server, SecurityStartTLS)
	cfg.Username = "apikey"
	cfg.Password = "s3cret-smtp-password"

	if err := Send(context.Background(), cfg, Message{To: "admin@example.com", Subject: "s", Body: "b"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !server.sawTLS() {
		t.Error("the server never completed a TLS handshake, so this send went out in plaintext")
	}
	creds := server.credentials()
	if len(creds) != 1 || !strings.Contains(creds[0], "s3cret-smtp-password") {
		// The credential is expected to reach the SERVER - that is what
		// authentication is. What must never happen is it reaching an
		// error or a log, which the leak test below covers.
		t.Fatalf("the server saw credentials %q, want one PLAIN credential carrying the password", creds)
	}
	if len(server.messages()) != 1 {
		t.Errorf("the server received %d messages, want 1", len(server.messages()))
	}
}

func TestSend_ImplicitTLSWrapsTheConnectionFromTheFirstByte(t *testing.T) {
	tlsConfig := selfSignedTLS(t)
	server := newFake(t, func(s *fakeSMTP) {
		s.tlsConfig = tlsConfig
		s.implicitTLS = true
	})

	if err := Send(context.Background(), configFor(server, SecurityTLS), Message{To: "admin@example.com", Subject: "s", Body: "b"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !server.sawTLS() {
		t.Error("the server never completed a TLS handshake for an implicit-TLS send")
	}
	if len(server.messages()) != 1 {
		t.Errorf("the server received %d messages, want 1", len(server.messages()))
	}
}

// The property the whole recovery feature rests on: a failing send is a
// failing send, and its error says where it failed without ever quoting
// the credential that failed with it.
func TestSend_RefusalsAreReportedWithoutTheCredential(t *testing.T) {
	const password = "s3cret-smtp-password"
	for _, tc := range []struct {
		name     string
		refuseAt string
		wantIn   string
	}{
		{"authentication", "AUTH", "authenticating to"},
		{"sender", "MAIL", "refused the from-address"},
		{"recipient", "RCPT", "refused the recipient"},
		{"message", "DATA-END", "did not accept the message"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tlsConfig := selfSignedTLS(t)
			server := newFake(t, func(s *fakeSMTP) {
				s.tlsConfig = tlsConfig
				s.offerStartTLS = true
				s.refuseAt = tc.refuseAt
			})
			cfg := configFor(server, SecurityStartTLS)
			cfg.Username = "apikey"
			cfg.Password = password

			err := Send(context.Background(), cfg, Message{To: "admin@example.com", Subject: "s", Body: "b"})
			if err == nil {
				t.Fatalf("Send succeeded against a server refusing at %s", tc.refuseAt)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not name the stage that failed (%q)", err, tc.wantIn)
			}
			if strings.Contains(err.Error(), password) {
				t.Fatalf("the SMTP password appears in the error text: %q", err)
			}
			if strings.Contains(err.Error(), "apikey") {
				t.Errorf("the SMTP username appears in the error text: %q", err)
			}
		})
	}
}

func TestSend_UnreachableHostFailsRatherThanHanging(t *testing.T) {
	// A port nothing listens on: bind one, learn its number, release it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	cfg := Config{Host: "127.0.0.1", Port: addr.Port, Security: SecurityNone, From: "backupd@example.com"}
	err = Send(context.Background(), cfg, Message{To: "admin@example.com", Subject: "s", Body: "b"})
	if err == nil {
		t.Fatal("Send succeeded against a port nothing is listening on")
	}
	if !strings.Contains(err.Error(), "connecting to") {
		t.Errorf("error %q does not report the connection as what failed", err)
	}
}

// net/smtp's own refusal to hand PLAIN credentials to a connection it has
// not encrypted, asserted here because this package relies on it rather
// than re-implementing it - and because the one exemption matters: the
// standard library allows it to a LOOPBACK server, which is exactly the
// "relay running on this same host" deployment SecurityNone exists for.
// The non-loopback case is the one that must fail, and it is proved
// against a server that would happily have recorded the credential.
func TestSend_PlaintextConnectionRefusesToCarryCredentialsOffHost(t *testing.T) {
	server := newFake(t, func(s *fakeSMTP) {})
	cfg := configFor(server, SecurityNone)
	// Resolves to the loopback listener (hosts files everywhere map it),
	// while being a NAME rather than 127.0.0.1, which is what takes it
	// out of net/smtp's loopback exemption.
	cfg.Host = "localhost.localdomain"
	cfg.Username = "apikey"
	cfg.Password = "s3cret-smtp-password"

	err := Send(context.Background(), cfg, Message{To: "admin@example.com", Subject: "s", Body: "b"})
	if err == nil {
		t.Fatal("Send handed a password to an unencrypted connection to a non-loopback host")
	}
	if strings.Contains(err.Error(), "s3cret-smtp-password") {
		t.Fatalf("the password appears in the refusal: %q", err)
	}
	if creds := server.credentials(); len(creds) != 0 {
		t.Fatalf("the server saw credentials %q over an unencrypted connection", creds)
	}
}

func TestValidate_RefusesConfigurationsThatCouldNeverSend(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no host":          {Port: 25, Security: SecurityNone, From: "a@b.com"},
		"port zero":        {Host: "h", Security: SecurityNone, From: "a@b.com"},
		"port too large":   {Host: "h", Port: 70000, Security: SecurityNone, From: "a@b.com"},
		"unknown security": {Host: "h", Port: 25, Security: Security("ssl"), From: "a@b.com"},
		"no from":          {Host: "h", Port: 25, Security: SecurityNone},
		"from with name":   {Host: "h", Port: 25, Security: SecurityNone, From: "Backupd <a@b.com>"},
		"injected host":    {Host: "h\r\nEHLO evil", Port: 25, Security: SecurityNone, From: "a@b.com"},
	} {
		t.Run(name, func(t *testing.T) {
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %+v", cfg)
			}
			if !errors.Is(err, ErrIncomplete) {
				t.Errorf("Validate returned %v, which is not an ErrIncomplete", err)
			}
		})
	}

	ok := Config{Host: "smtp.example.com", Port: 587, Security: SecurityStartTLS, From: "a@b.com"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate refused a complete configuration: %v", err)
	}
}

// The header-injection boundary, asserted against what the server actually
// received rather than against Validate alone: a subject or recipient
// carrying CRLF must never become extra headers.
func TestSend_RefusesHeaderInjectionThroughTheRecipientOrSubject(t *testing.T) {
	server := newFake(t, func(s *fakeSMTP) {})
	cfg := configFor(server, SecurityNone)

	for name, msg := range map[string]Message{
		"recipient":  {To: "admin@example.com\r\nBcc: attacker@example.net", Subject: "s", Body: "b"},
		"subject":    {To: "admin@example.com", Subject: "s\r\nBcc: attacker@example.net", Body: "b"},
		"no subject": {To: "admin@example.com", Subject: "   ", Body: "b"},
	} {
		t.Run(name, func(t *testing.T) {
			err := Send(context.Background(), cfg, msg)
			if err == nil {
				t.Fatalf("Send accepted %+v", msg)
			}
			if !errors.Is(err, ErrIncomplete) {
				t.Errorf("Send returned %v, which is not an ErrIncomplete", err)
			}
		})
	}
	if msgs := server.messages(); len(msgs) != 0 {
		t.Fatalf("the server received %d messages from refused sends: %q", len(msgs), msgs)
	}
}

func TestValidateAddress(t *testing.T) {
	for _, addr := range []string{"admin@example.com", "first.last+tag@sub.example.co.uk", "root@localhost"} {
		if err := ValidateAddress(addr); err != nil {
			t.Errorf("ValidateAddress(%q) = %v, want nil", addr, err)
		}
	}
	for _, addr := range []string{"", "   ", "not-an-address", "a@", "@b.com", "a@b.com, c@d.com", "A <a@b.com>", "a@b.com\r\nBcc: x@y.z"} {
		if err := ValidateAddress(addr); err == nil {
			t.Errorf("ValidateAddress(%q) = nil, want a refusal", addr)
		}
	}
}

// A Sender is the seam handlers take; this pins that Send itself satisfies
// it, so a signature change cannot pass here and fail at every call site.
func TestSendSatisfiesSender(t *testing.T) {
	var s Sender = Send
	if s == nil {
		t.Fatal("Send is not assignable to Sender")
	}
	if got := fmt.Sprintf("%T", s); got == "" {
		t.Fatal("unreachable")
	}
}
