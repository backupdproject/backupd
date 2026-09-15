package remoteexec

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/retnd/retnd/core/internal/config"
	"github.com/retnd/retnd/core/internal/transport"
)

// ErrConnection is every refusal about which connection a remote step runs
// over: a reference that resolves to nothing, a connection that cannot be
// used for exec by construction, or a bash path that could not be a fixed
// command.
//
// One sentinel for the class, because the caller's decision is the same for
// all of them: this step's validation fails, and the backup carries on.
var ErrConnection = errors.New("remoteexec: this remote execution connection cannot be used")

// DefaultBashPath is the shell a remote hook is run with when the
// connection does not name one.
//
// A fixed absolute path rather than a PATH lookup, and /bin/bash rather
// than /bin/sh. Both halves are deliberate: a PATH lookup on the far side
// is resolved by the remote account's own environment, which is the thing
// the envelope exists to take out of the decision, and #810's technical
// requirements say never to silently substitute /bin/sh or another shell.
// A host whose bash is somewhere else says so, per connection.
const DefaultBashPath = "/bin/bash"

// bashPathRule is what a configured bash path must look like before it may
// become part of a remote command string.
//
// The remote command is interpreted by the remote account's LOGIN SHELL, so
// a path containing a space, a quote, a semicolon or a dollar sign would
// not be a path any more -- it would be additional shell syntax this
// product sent. There is no quoting applied to it and deliberately so: a
// value that needs quoting to be safe is a value this package refuses.
var bashPathRule = regexp.MustCompile(`^/[A-Za-z0-9._/+-]*[A-Za-z0-9._+-]$`)

// ConnectionKind is where a resolved connection came from, for the audit
// line and for the refusal wording.
type ConnectionKind string

const (
	// KindDeclared is a workflows.exec_connections entry: an execution
	// connection with its own credential.
	KindDeclared ConnectionKind = "declared"
	// KindBackupSetSource is a backup set's own source connection, reused
	// for execution. It is exactly the case that has to be proven
	// exec-capable rather than assumed, because that credential's job is
	// transferring artifacts.
	KindBackupSetSource ConnectionKind = "backup-set-source"
)

// Connection is a resolved remote execution connection: everything needed
// to open an exec channel, and nothing about what to run on it.
type Connection struct {
	// Ref is the reference that named this connection, as configured. It
	// goes in the audit line, so an operator reading one can find the
	// configuration it came from.
	Ref string

	// Kind is which of the two resolution rules produced it.
	Kind ConnectionKind

	// Source carries the endpoint and the credential: host, port, user,
	// the File/Env/Command key reference, the known_hosts policy and the
	// connection ceiling. It is a transport.Source rather than a new type
	// so that internal/transport/rclone's custody code -- the mode and
	// directory-chain checks, the at-rest decryption, the passphrase
	// resolution -- applies unchanged.
	Source transport.Source

	// BashPath is the shell the envelope invokes. Empty means
	// DefaultBashPath.
	BashPath string
}

// Bash is the path this connection's hooks run under.
func (c Connection) Bash() string {
	if c.BashPath == "" {
		return DefaultBashPath
	}

	return c.BashPath
}

// Validate reports the first reason this connection could not carry a
// remote hook, before anything opens a socket.
func (c Connection) Validate() error {
	if c.Ref == "" {
		return fmt.Errorf("%w: it has no reference, so nothing in an audit line could say which configuration it came from", ErrConnection)
	}
	if c.Source.Type != "sftp" {
		return fmt.Errorf("%w: %q is of type %q, and a remote execution connection is an SSH connection (type \"sftp\"); a hook meant for this backup server is a NAME.local.sh instead",
			ErrConnection, c.Ref, c.Source.Type)
	}
	if c.Source.Host == "" {
		return fmt.Errorf("%w: %q names no host", ErrConnection, c.Ref)
	}
	if c.Source.User == "" {
		return fmt.Errorf("%w: %q names no SSH user, and a hook runs as the configured user -- this product never escalates to pick one", ErrConnection, c.Ref)
	}
	if c.Source.Port < 0 || c.Source.Port > 65535 {
		return fmt.Errorf("%w: %q has port %d, which is out of range", ErrConnection, c.Ref, c.Source.Port)
	}
	if c.Source.MaxConnections < 0 {
		return fmt.Errorf("%w: %q has a negative connection ceiling (%d)", ErrConnection, c.Ref, c.Source.MaxConnections)
	}
	if !bashPathRule.MatchString(c.Bash()) {
		return fmt.Errorf("%w: %q names the bash path %q, which cannot be used as a fixed remote command: it must be an absolute path made of letters, digits, dot, dash, plus, underscore and slash. A path needing shell quoting to be safe is refused rather than quoted",
			ErrConnection, c.Ref, c.Bash())
	}

	return nil
}

// Resolve turns a step's execution connection reference into a Connection.
//
// The two resolution rules are #810's modelling decision, and the ORDER is
// part of it: a declared execution connection wins over a backup set of the
// same name. That is unambiguous rather than arbitrary, because
// internal/config refuses a declared id containing "/", which is the only
// way a backup set is ever named.
//
// Nothing here contacts anything. Whether the connection this returns can
// actually run a command is Preflight's question, against the server,
// because it is the one question no configuration file can answer
// truthfully.
func Resolve(cfg *config.Config, ref string) (Connection, error) {
	if cfg == nil {
		return Connection{}, fmt.Errorf("%w: there is no configuration to resolve %q against", ErrConnection, ref)
	}
	if ref == "" {
		return Connection{}, fmt.Errorf("%w: the step names no execution connection at all. A NAME.remote.sh runs on the host this backup set pulls from, and the connection it runs over has to be configured: set the backup set's workflow remote_exec_connection_ref, or rename the script to NAME.local.sh to run it on this backup server instead",
			ErrConnection)
	}

	if declared, ok := cfg.WorkflowExecConnection(ref); ok {
		conn := Connection{
			Ref:    ref,
			Kind:   KindDeclared,
			Source: sourceFromRemote(cfg, "workflow-exec/"+ref, declared.Remote),
		}

		return conn, conn.Validate()
	}

	if set, ok := backupSetNamed(cfg, ref); ok {
		conn := Connection{
			Ref:    ref,
			Kind:   KindBackupSetSource,
			Source: sourceFromRemote(cfg, ref, set.Remote),
		}

		return conn, conn.Validate()
	}

	return Connection{}, fmt.Errorf("%w: %q names no execution connection and no backup set. %s",
		ErrConnection, ref, describeAvailable(cfg))
}

// backupSetNamed finds the backup set a "source/set" reference names.
func backupSetNamed(cfg *config.Config, ref string) (config.BackupSet, bool) {
	sourceName, setName, found := strings.Cut(ref, "/")
	if !found {
		return config.BackupSet{}, false
	}

	for _, src := range cfg.Sources {
		if src.Name != sourceName {
			continue
		}
		for _, bs := range src.BackupSets {
			if bs.Name == setName {
				return bs, true
			}
		}
	}

	return config.BackupSet{}, false
}

// sourceFromRemote translates a configured Remote into the transport.Source
// the credential code reads.
//
// It is deliberately not internal/app's sourceFor. That function also
// carries RemotePath and ExcludePaths, which are facts about a TRANSFER --
// which tree to walk, what to skip -- and an execution connection has no
// tree. Forwarding them would put a backup set's remote path into a
// connection whose only job is to open a channel, where the only thing it
// could ever do is mislead a reader.
//
// KeyEncryption comes from the deployment rather than the remote, exactly
// as it does there: it is a config-wide setting (#298) that travels on
// every Source because internal/transport/rclone is the only place that can
// act on it.
func sourceFromRemote(cfg *config.Config, id string, r config.Remote) transport.Source {
	return transport.Source{
		ID:                   id,
		Type:                 r.Type,
		Host:                 r.Host,
		Port:                 r.Port,
		User:                 r.User,
		MaxConnections:       r.MaxConnections,
		KeyFile:              r.Key.File,
		KeyEnv:               r.Key.Env,
		KeyCommand:           r.Key.Command,
		PassphraseFile:       r.Key.Passphrase.File,
		PassphraseEnv:        r.Key.Passphrase.Env,
		PassphraseCommand:    r.Key.Passphrase.Command,
		KeyEncryptionFile:    cfg.KeyEncryption.File,
		KeyEncryptionEnv:     cfg.KeyEncryption.Env,
		KeyEncryptionCommand: cfg.KeyEncryption.Command,
		KnownHosts:           r.KnownHosts,
	}
}

// describeAvailable lists what a reference COULD have named, because the
// most likely cause of this refusal is a typo and the fix is in front of
// whoever reads it.
func describeAvailable(cfg *config.Config) string {
	var declared, sets []string
	for _, conn := range cfg.Workflows.ExecConnections {
		declared = append(declared, conn.Name)
	}
	for _, src := range cfg.Sources {
		for _, bs := range src.BackupSets {
			sets = append(sets, src.Name+"/"+bs.Name)
		}
	}
	sort.Strings(declared)
	sort.Strings(sets)

	parts := make([]string, 0, 2)
	if len(declared) != 0 {
		parts = append(parts, "declared execution connections: "+strings.Join(declared, ", "))
	} else {
		parts = append(parts, "no execution connections are declared")
	}
	if len(sets) != 0 {
		parts = append(parts, "backup sets: "+strings.Join(sets, ", "))
	}

	return strings.Join(parts, "; ")
}
