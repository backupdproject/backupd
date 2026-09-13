// Package email sends one message over one operator-configured SMTP
// connection, and does nothing else.
//
// # Why this exists, and why it is this small
//
// Account recovery (issue #830) needs the runtime to be able to email the
// administrator: a confirmation at enrollment that proves the SMTP
// connection works, and a reset link when the password is forgotten. That
// is three short messages a year on a home NAS, to one recipient, over a
// connection the operator typed in themselves - so what it needs is a
// submission client, not a mail stack. net/smtp is the whole dependency,
// deliberately: a provider SDK would add a network client, a credential
// model and a release cadence to a product whose entire use of email is
// "connect to the host the operator named and hand over one message."
//
// # What it refuses to do
//
// It never retries. A send either worked or it did not, and the caller is
// the one that knows whether the answer belongs in an HTTP refusal
// (enrollment and the Settings test send, where the operator is waiting
// and has to see the SMTP error) or in a log line (forgot-password, where
// the response must not vary at all). A retry loop here would make the
// first of those hang and the second lie.
//
// It never puts credentials in an error. Every failure names the host, the
// port and the stage that failed, because that is what an operator has to
// go and fix, and it names nothing else. Config.Password is not rendered
// by anything in this package, not even on the path where authentication
// is what failed.
//
// It never reads a message body from a caller's header input. To, Subject
// and From are checked for CR and LF before a single header is written
// (Message.Validate/Config.Validate), so a recovery address that arrived
// over the wire cannot append headers of its own to the message this
// package composes.
package email

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Security is how a connection to the submission host is protected.
//
// There are exactly three because there are exactly three an operator is
// ever told to pick from by a mail provider: submission on 587 with
// STARTTLS, implicit TLS on 465, and plaintext, which is only ever
// reasonable for a relay on the same host or the same private network.
type Security string

const (
	// SecurityStartTLS connects in plaintext and upgrades with STARTTLS
	// before authenticating. This is what SMTP2go, Gmail and essentially
	// every hosted provider document for port 587.
	SecurityStartTLS Security = "starttls"

	// SecurityTLS wraps the connection in TLS from the first byte
	// ("SMTPS", conventionally port 465).
	SecurityTLS Security = "tls"

	// SecurityNone sends in plaintext and never upgrades. Authentication
	// over such a connection is refused by net/smtp itself (see Send),
	// which is the correct answer rather than an inconvenience.
	SecurityNone Security = "none"
)

// Securities is every accepted Security value, in the order a form should
// offer them. api/v1/openapi.json's SmtpSettings.security enum and
// ui/shared's own select are held to this list.
var Securities = []Security{SecurityStartTLS, SecurityTLS, SecurityNone}

// Valid reports whether s is one of the three accepted values.
func (s Security) Valid() bool {
	for _, known := range Securities {
		if s == known {
			return true
		}
	}
	return false
}

// Config is one submission endpoint: where to connect, how to protect the
// connection, who to authenticate as, and what address the mail is from.
//
// Password is present in this struct and in memory for the duration of one
// send, and that is the only place it is ever allowed to be: nothing in
// this package logs it, formats it or returns it, and the caller
// (apps/common/auth/local) persists a reference to it rather than the
// value.
type Config struct {
	Host     string
	Port     int
	Security Security
	Username string
	Password string
	From     string
}

// Addr is the host:port this Config dials.
func (c Config) Addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

// ErrIncomplete is the class of refusal a Config or Message that could
// never be sent falls into. It is distinct from a send failure because the
// two have different callers: an incomplete configuration is a validation
// error against the operator's own input, while a send failure is a report
// about somebody else's mail server.
var ErrIncomplete = errors.New("email: incomplete")

// Validate refuses a Config that could not possibly send: no host, a port
// outside 1-65535, an unknown security mode, no from-address, a
// from-address that is not an address, or header-injecting control
// characters anywhere.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("%w: an SMTP host is required", ErrIncomplete)
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("%w: %d is not an SMTP port", ErrIncomplete, c.Port)
	}
	if !c.Security.Valid() {
		return fmt.Errorf("%w: %q is not one of starttls, tls or none", ErrIncomplete, string(c.Security))
	}
	if err := ValidateAddress(c.From); err != nil {
		return fmt.Errorf("%w: from-address: %v", ErrIncomplete, err)
	}
	for name, value := range map[string]string{"host": c.Host, "username": c.Username} {
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%w: %s contains a line break", ErrIncomplete, name)
		}
	}
	return nil
}

// Message is one plaintext message to one recipient.
//
// One recipient rather than a list, because every message this product
// sends goes to the administrator's own recovery address, and a list would
// be a feature with no caller and a fan-out to get wrong.
type Message struct {
	To      string
	Subject string
	Body    string
}

// Validate refuses a Message that could not be sent or that would inject
// headers: no recipient, a recipient that is not an address, an empty
// subject, or CR/LF in the subject.
func (m Message) Validate() error {
	if err := ValidateAddress(m.To); err != nil {
		return fmt.Errorf("%w: recipient: %v", ErrIncomplete, err)
	}
	if strings.TrimSpace(m.Subject) == "" {
		return fmt.Errorf("%w: a subject is required", ErrIncomplete)
	}
	if strings.ContainsAny(m.Subject, "\r\n") {
		return fmt.Errorf("%w: the subject contains a line break", ErrIncomplete)
	}
	return nil
}

// ValidateAddress reports whether addr is a single bare email address -
// "someone@example.com", not "Someone <someone@example.com>" and not two
// addresses.
//
// The display-name form is refused rather than accepted-and-stripped
// because this is what an operator types into a one-line field, and a
// field that silently accepts more than it displays is how a recovery
// address ends up being something other than what somebody read back to
// themselves. net/mail.ParseAddress does the parsing, so the rule is the
// standard library's RFC 5322 reading rather than a regex this package
// invented.
func ValidateAddress(addr string) error {
	if strings.TrimSpace(addr) == "" {
		return errors.New("an email address is required")
	}
	if strings.ContainsAny(addr, "\r\n") {
		return errors.New("that address contains a line break")
	}
	parsed, err := mail.ParseAddress(addr)
	if err != nil {
		return fmt.Errorf("%q is not a valid email address", addr)
	}
	if parsed.Name != "" || parsed.Address != addr {
		return fmt.Errorf("%q must be a bare email address, without a display name", addr)
	}
	return nil
}

// Sender is the seam every caller takes this package through, so that a
// test can prove what its own handler does with a send that fails without
// standing up a mail server for it, while production wires Send.
type Sender func(ctx context.Context, cfg Config, msg Message) error

// sendTimeout bounds one whole send - dial, handshake, authenticate,
// transfer, quit. It is deliberately short: the two callers that surface
// the result to an operator (enrollment, the Settings test send) hold an
// HTTP request open while this runs, so a mail host that accepts a
// connection and then stops talking must become a refusal somebody can
// read rather than a page that never finishes.
const sendTimeout = 20 * time.Second

// tlsConfigFor is the TLS configuration this package uses for both
// SecurityTLS and the STARTTLS upgrade. It is a variable so this package's
// own test can point it at a self-signed certificate it generated for an
// in-process server; production never replaces it, and a caller outside
// this package cannot.
var tlsConfigFor = func(host string) *tls.Config {
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

// Send delivers msg over cfg, once.
//
// Verification is the standard library's: certificates are checked against
// the host's system roots with cfg.Host as the expected name, and there is
// deliberately no option to skip that. An operator whose provider has a
// certificate this host does not trust has a problem to fix at one of
// those two ends; a switch here would turn that problem into a silently
// unauthenticated connection carrying a password.
//
// Authentication is attempted only when Username is set, and only PLAIN is
// offered, because net/smtp's PlainAuth is the one mechanism every hosted
// provider accepts and it refuses to hand credentials to a connection that
// is not encrypted, or to a server other than the one named. That refusal
// is why SecurityNone plus a username fails here rather than leaking a
// password onto a plaintext link - with the standard library's one
// deliberate exemption, a LOOPBACK server, which is the "relay on this
// same host" case SecurityNone exists to serve and where there is no
// network for the credential to cross.
func Send(ctx context.Context, cfg Config, msg Message) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := msg.Validate(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", cfg.Addr())
	if err != nil {
		return fmt.Errorf("email: connecting to %s: %w", cfg.Addr(), err)
	}
	// One deadline for the whole conversation, derived from the same
	// context the dial used: net/smtp has no context of its own, so
	// without this a server that completes the TCP handshake and then
	// goes quiet would hold the connection - and the HTTP request waiting
	// on it - open indefinitely.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// Closed unconditionally. Quit below closes it on the success path,
	// and a second Close on an already-closed connection is a no-op; what
	// this covers is every early return between here and there.
	defer func() { _ = conn.Close() }()

	if cfg.Security == SecurityTLS {
		tlsConn := tls.Client(conn, tlsConfigFor(cfg.Host))
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("email: TLS handshake with %s: %w", cfg.Addr(), err)
		}
		conn = tlsConn
	}

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return fmt.Errorf("email: SMTP greeting from %s: %w", cfg.Addr(), err)
	}
	defer func() { _ = client.Close() }()

	if cfg.Security == SecurityStartTLS {
		if err := client.StartTLS(tlsConfigFor(cfg.Host)); err != nil {
			return fmt.Errorf("email: STARTTLS with %s: %w", cfg.Addr(), err)
		}
	}

	if cfg.Username != "" {
		// The error is wrapped without the password, and without the
		// username either: an operator who mistyped one of the two is
		// looking at the field they typed it into, and an authentication
		// failure echoed back into a log is how a credential ends up in
		// a support bundle.
		if err := client.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return fmt.Errorf("email: authenticating to %s: %w", cfg.Addr(), err)
		}
	}

	if err := client.Mail(addressOf(cfg.From)); err != nil {
		return fmt.Errorf("email: %s refused the from-address: %w", cfg.Addr(), err)
	}
	if err := client.Rcpt(addressOf(msg.To)); err != nil {
		return fmt.Errorf("email: %s refused the recipient: %w", cfg.Addr(), err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("email: %s refused the message: %w", cfg.Addr(), err)
	}
	if _, err := w.Write(render(cfg, msg)); err != nil {
		return fmt.Errorf("email: writing the message to %s: %w", cfg.Addr(), err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("email: %s did not accept the message: %w", cfg.Addr(), err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("email: closing the session with %s: %w", cfg.Addr(), err)
	}
	return nil
}

// addressOf is the envelope form of an already-validated address. Both
// callers have been through ValidateAddress, so this is a narrowing rather
// than a parse, and a parse failure cannot reach it.
func addressOf(addr string) string {
	parsed, err := mail.ParseAddress(addr)
	if err != nil {
		return addr
	}
	return parsed.Address
}

// render composes the RFC 5322 message.
//
// CRLF everywhere, because SMTP's data transfer is defined in terms of it
// and a bare LF is what makes a message arrive with its headers folded
// into the body on some servers. The body's own line endings are
// normalised for the same reason, and a leading "." is escaped by
// net/smtp's own DataWriter, which is why nothing here does it twice.
//
// There is no Message-ID. This package has no hostname it can honestly
// claim one from, and a submission service assigns one itself; inventing
// one from the SMTP host would put a lie in a header rather than fill a
// gap.
func render(cfg Config, msg Message) []byte {
	var b strings.Builder
	b.WriteString("From: " + cfg.From + "\r\n")
	b.WriteString("To: " + msg.To + "\r\n")
	b.WriteString("Subject: " + msg.Subject + "\r\n")
	b.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	body := strings.ReplaceAll(msg.Body, "\r\n", "\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	if !strings.HasSuffix(b.String(), "\r\n") {
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}
