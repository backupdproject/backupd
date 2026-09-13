package remoteexec

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/backupdproject/backupd/core/internal/transport/rclone"
)

// ErrHostKeyPolicy is a host identity that does not match what this
// deployment pinned.
//
// It is its own sentinel because it is the one failure that must never be
// retried, worked around or degraded into a warning: a host offering a key
// known_hosts does not vouch for is either a host this deployment has not
// been told about or a host in the middle, and a hook is arbitrary code
// being handed to it.
var ErrHostKeyPolicy = errors.New("remoteexec: the remote host's identity does not match the pinned policy")

// ErrTransportLoss is the connection or channel failing, as opposed to the
// hook failing.
//
// The distinction is a technical requirement of #810 and it is not
// cosmetic: a hook that exits 1 has run and decided something, while a
// connection that dropped mid-step has left the remote side in a state
// nobody observed. Reporting the second as an exit code would let a
// dropped connection be read as "the pre-backup quiesce script reported
// failure", which is a different afternoon.
var ErrTransportLoss = errors.New("remoteexec: the SSH connection or exec channel failed, so no exit status was observed")

// ErrExecCapability is a connection that authenticates but may not run a
// command: an internal-sftp-forced account, a forced-command account, a
// host with no bash where the connection says there is one.
//
// It is separate from ErrConnection because the two have different fixes
// and different blast radii. A configuration error is fixed in the config
// file; a capability refusal means the account is doing exactly what it was
// hardened to do, and the fix is a separate execution connection -- while
// artifact backup over that same account carries on working.
var ErrExecCapability = errors.New("remoteexec: this connection authenticates but is not exec-capable")

const (
	// dialTimeout bounds the TCP connect and the SSH handshake together.
	// ssh.ClientConfig.Timeout covers only the first of them (it is
	// documented as "the maximum amount of time for the TCP connection to
	// establish"), which is the gap core/tests/machines' own handshake
	// probe exists to close, so the handshake is bounded here instead.
	dialTimeout = 30 * time.Second

	// keepAliveInterval is how often the client asks the server for a
	// reply while a long hook runs. A hook is allowed to be quiet for its
	// whole timeout, and a NAT or firewall that drops an idle connection
	// would otherwise turn a legitimately slow quiesce into a transport
	// loss with nothing to point at.
	keepAliveInterval = 30 * time.Second
)

// tokenRule is what a step token must look like before it may become part
// of the remote command string. See Request.Token.
var tokenRule = regexp.MustCompile(`^[A-Za-z0-9._-]{1,120}$`)

// Client is one SSH connection to one execution connection's host.
//
// It holds exactly one TCP connection and opens sessions on it. That is the
// connection ceiling honoured by construction: a Source configured with
// max_connections: 1 -- the production rule behind #264, where a third
// simultaneous connection from one address is rejected with a TCP reset --
// is satisfied without the ceiling having to be consulted, because there is
// never a second connection to make unless the first one has died.
type Client struct {
	conn   Connection
	signer ssh.Signer
	verify ssh.HostKeyCallback
	addr   string

	mu     sync.Mutex
	client *ssh.Client
	// hostKey is what the server actually presented, recorded during the
	// handshake for the audit line. It is the endpoint's identity, never a
	// credential.
	hostKey string
}

// Dial opens the connection and verifies the host's identity against the
// pinned known_hosts before anything else happens.
//
// Host-key verification is knownhosts' own callback over the very file the
// transport verifies against, reached through rclone.SourceKnownHostsFile
// so the two cannot end up reading the setting differently. That is
// core/service/connectiontest.go's argument, and it is stronger here: a
// reading that compared fingerprints itself would treat an @revoked line as
// trusted and count a key pinned for another host as this host's.
func Dial(ctx context.Context, conn Connection) (*Client, error) {
	if err := conn.Validate(); err != nil {
		return nil, err
	}

	knownHostsPath, err := rclone.SourceKnownHostsFile(conn.Source)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrConnection, err)
	}
	verify, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("%w: the pinned host keys in %s could not be read, and \"I could not check\" is the outcome an attacker would choose: %v",
			ErrHostKeyPolicy, knownHostsPath, err)
	}

	signer, err := rclone.SourceSigner(conn.Source)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrConnection, err)
	}

	port := conn.Source.Port
	if port == 0 {
		port = 22
	}

	c := &Client{
		conn:   conn,
		signer: signer,
		verify: verify,
		addr:   net.JoinHostPort(conn.Source.Host, strconv.Itoa(port)),
	}
	if err := c.connect(ctx); err != nil {
		return nil, err
	}

	return c, nil
}

// connect performs one dial and handshake, bounded by both the context and
// dialTimeout.
func (c *Client) connect(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	var dialer net.Dialer
	tcp, err := dialer.DialContext(dialCtx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("%w: connecting to %s: %v", ErrTransportLoss, c.addr, err)
	}

	// The handshake gets the rest of the same budget. Without this
	// deadline a peer that accepts the connection and then says nothing
	// holds the dial open indefinitely: ssh.NewClientConn takes no context
	// and reads no timeout of its own.
	if deadline, ok := dialCtx.Deadline(); ok {
		_ = tcp.SetDeadline(deadline)
	}

	var presented string
	cfg := &ssh.ClientConfig{
		User: c.conn.Source.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(c.signer)},
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			presented = ssh.FingerprintSHA256(key)

			return c.verify(hostname, remote, key)
		},
		Timeout: dialTimeout,
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(tcp, c.addr, cfg)
	if err != nil {
		_ = tcp.Close()
		if isHostKeyFailure(err) {
			return fmt.Errorf("%w: %s offered a host key this deployment has not pinned (%s): %v",
				ErrHostKeyPolicy, c.addr, presented, err)
		}

		return fmt.Errorf("%w: the SSH handshake with %s failed: %v", ErrTransportLoss, c.addr, err)
	}
	_ = tcp.SetDeadline(time.Time{})

	client := ssh.NewClient(sshConn, chans, reqs)

	c.mu.Lock()
	c.client = client
	c.hostKey = presented
	c.mu.Unlock()

	go c.keepAlive(client)

	return nil
}

// keepAlive asks the server for a reply periodically for as long as this
// connection lives. The request name is the one OpenSSH ignores politely
// (any unknown global request gets a failure reply, which is all this
// needs: a reply proves the path is still there).
func (c *Client) keepAlive(client *ssh.Client) {
	ticker := time.NewTicker(keepAliveInterval)
	defer ticker.Stop()

	for range ticker.C {
		if _, _, err := client.SendRequest("keepalive@backupd", true, nil); err != nil {
			return
		}
	}
}

// HostKeyFingerprint is the identity the server actually presented, for the
// audit line. It is a public fingerprint, never a credential.
func (c *Client) HostKeyFingerprint() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.hostKey
}

// Close releases the connection.
func (c *Client) Close() error {
	c.mu.Lock()
	client := c.client
	c.client = nil
	c.mu.Unlock()

	if client == nil {
		return nil
	}

	return client.Close()
}

// session opens one exec session on the live connection.
func (c *Client) session() (*ssh.Session, error) {
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()

	if client == nil {
		return nil, fmt.Errorf("%w: the connection to %s is closed", ErrTransportLoss, c.addr)
	}

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("%w: opening an exec channel on %s: %v", ErrTransportLoss, c.addr, err)
	}

	return session, nil
}

// remoteCommand is the fixed command every exec session on this connection
// starts, and the ONLY string this package ever sends as a command.
//
// Everything in it is either a constant or a value that has already been
// held to a character rule: the bash path by Connection.Validate, the token
// by Request.validate. Nothing an operator can write and nothing from a
// hook's environment reaches it, which is the whole point -- the remote
// command line is world-readable in the remote process list, so a secret or
// an environment value placed here would be published to every account on
// that host.
//
// `exec` replaces the login shell rather than leaving it as a parent, so
// nothing of this product's own remains in the remote process tree. The
// token is a positional argument: bash with -s treats operands as the
// script's positional parameters rather than as a file to run, so it
// changes nothing about execution while giving the reaper something to
// recognise the step's process group by.
func (c *Client) remoteCommand(token string) string {
	cmd := "exec " + c.conn.Bash() + " --noprofile --norc -s"
	if token != "" {
		cmd += " " + token
	}

	return cmd
}

// isHostKeyFailure reports whether err is the handshake refusing the
// server's key rather than any other handshake failure.
//
// x/crypto/ssh reports it as *knownhosts.KeyError when the file had
// something to say about this host, and as a plain error mentioning the
// host key when it did not. Both are host-key failures and both must be
// reported as ErrHostKeyPolicy: the alternative is a mismatch surfacing as
// a generic transport error, which is the one failure a retry must never
// paper over.
func isHostKeyFailure(err error) bool {
	var keyErr *knownhosts.KeyError
	if errors.As(err, &keyErr) {
		return true
	}
	var revoked *knownhosts.RevokedError
	if errors.As(err, &revoked) {
		return true
	}

	return containsHostKeyWording(err)
}

func containsHostKeyWording(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, wording := range []string{"host key", "hostkey", "knownhosts"} {
		if strings.Contains(msg, wording) {
			return true
		}
	}

	return false
}
