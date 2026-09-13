package remoteexec

import (
	"errors"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/transport"
)

func testConfig() *config.Config {
	return &config.Config{
		Workflows: config.Workflows{
			Root: "/workflows",
			ExecConnections: []config.WorkflowExecConnection{{
				Name: "db-hooks",
				Remote: config.Remote{
					Type:       "sftp",
					Host:       "db.example.com",
					Port:       2222,
					User:       "backupd-hooks",
					KnownHosts: "/etc/backupd/known_hosts",
					Key:        config.Key{File: "/etc/backupd/hooks.key"},
				},
			}},
		},
		KeyEncryption: config.KeyEncryption{File: "/etc/backupd/at-rest.key"},
		Sources: []config.Source{{
			Name: "production",
			BackupSets: []config.BackupSet{{
				Name: "db",
				Remote: config.Remote{
					Type:           "sftp",
					Host:           "db.example.com",
					User:           "backupd-transfer",
					KnownHosts:     "/etc/backupd/known_hosts",
					Key:            config.Key{File: "/etc/backupd/transfer.key"},
					MaxConnections: 2,
				},
				RemotePath: "/var/backups",
			}},
		}},
	}
}

// TestResolvePrefersADeclaredExecConnection is #810's modelling decision:
// the execution connection is a different connection from the transfer one,
// so a reference that names a declared connection must resolve to THAT
// credential and not to the set's source.
func TestResolvePrefersADeclaredExecConnection(t *testing.T) {
	t.Parallel()

	conn, err := Resolve(testConfig(), "db-hooks")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if conn.Kind != KindDeclared {
		t.Errorf("kind = %q, want %q", conn.Kind, KindDeclared)
	}
	if conn.Source.User != "backupd-hooks" {
		t.Errorf("user = %q; the transfer credential was used for execution", conn.Source.User)
	}
	if conn.Source.KeyFile != "/etc/backupd/hooks.key" {
		t.Errorf("key = %q", conn.Source.KeyFile)
	}
	if conn.Source.Port != 2222 {
		t.Errorf("port = %d, so the configured port did not survive resolution", conn.Source.Port)
	}
	if conn.Source.KnownHosts == "" {
		t.Error("known_hosts did not survive resolution, and host-key verification is not optional")
	}
	// #298's at-rest key encryption is a deployment-wide setting and has
	// to reach the credential code, or an exec connection whose key file
	// is encrypted cannot be opened at all.
	if conn.Source.KeyEncryptionFile != "/etc/backupd/at-rest.key" {
		t.Errorf("key_encryption = %q", conn.Source.KeyEncryptionFile)
	}
	// An execution connection has no tree to walk, so it must not carry
	// one: a remote path here could only mislead whoever read it.
	if conn.Source.Root != "" {
		t.Errorf("the resolved execution connection carries a remote path (%q)", conn.Source.Root)
	}
}

// TestResolveFallsBackToTheBackupSetsOwnSourceConnection is the other half
// of the rule. It resolves; whether it may RUN anything is settled by a
// real capability preflight, which is why nothing here asserts it cannot.
func TestResolveFallsBackToTheBackupSetsOwnSourceConnection(t *testing.T) {
	t.Parallel()

	conn, err := Resolve(testConfig(), "production/db")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if conn.Kind != KindBackupSetSource {
		t.Errorf("kind = %q, want %q", conn.Kind, KindBackupSetSource)
	}
	if conn.Source.User != "backupd-transfer" {
		t.Errorf("user = %q", conn.Source.User)
	}
	if conn.Source.MaxConnections != 2 {
		t.Errorf("the connection ceiling did not survive resolution: %d", conn.Source.MaxConnections)
	}
}

func TestResolveRefusals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		ref     string
		mutate  func(*config.Config)
		mustSay string
	}{{
		name:    "a reference to nothing",
		ref:     "typo-hooks",
		mustSay: "names no execution connection and no backup set",
	}, {
		name:    "no reference at all",
		ref:     "",
		mustSay: "names no execution connection at all",
	}, {
		name: "a backup set whose source is local",
		ref:  "production/db",
		mutate: func(c *config.Config) {
			c.Sources[0].BackupSets[0].Remote = config.Remote{Type: "local"}
		},
		mustSay: "is an SSH connection",
	}, {
		name: "a connection with no user",
		ref:  "db-hooks",
		mutate: func(c *config.Config) {
			c.Workflows.ExecConnections[0].Remote.User = ""
		},
		mustSay: "names no SSH user",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig()
			if c.mutate != nil {
				c.mutate(cfg)
			}
			_, err := Resolve(cfg, c.ref)
			if err == nil {
				t.Fatal("this reference resolved")
			}
			if !errors.Is(err, ErrConnection) {
				t.Errorf("error %v is not an ErrConnection", err)
			}
			if !strings.Contains(err.Error(), c.mustSay) {
				t.Errorf("the refusal does not say %q: %v", c.mustSay, err)
			}
		})
	}
}

// TestResolveNamesWhatWasAvailable is about the diagnostic. The overwhelming
// cause of this refusal is a typo, and the fix is a name the operator
// already wrote somewhere else in the same file.
func TestResolveNamesWhatWasAvailable(t *testing.T) {
	t.Parallel()

	_, err := Resolve(testConfig(), "db-hoks")
	if err == nil {
		t.Fatal("a typo resolved")
	}
	for _, want := range []string{"db-hooks", "production/db"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not offer %q as a candidate: %v", want, err)
		}
	}
}

// TestBashPathMustBeUsableAsAFixedRemoteCommand is a security boundary, not
// a tidiness rule: the bash path is placed into a command string the remote
// LOGIN SHELL parses, with no quoting applied, so anything that is not a
// plain absolute path would be extra shell syntax this product sent.
func TestBashPathMustBeUsableAsAFixedRemoteCommand(t *testing.T) {
	t.Parallel()

	base := Connection{
		Ref:    "db-hooks",
		Kind:   KindDeclared,
		Source: transport.Source{Type: "sftp", Host: "h", User: "u", KnownHosts: "/k"},
	}

	if err := base.Validate(); err != nil {
		t.Fatalf("the default bash path was refused: %v", err)
	}
	if base.Bash() != DefaultBashPath {
		t.Errorf("Bash() = %q, want %q", base.Bash(), DefaultBashPath)
	}

	for _, path := range []string{
		"/usr/local/bin/bash",
		"/opt/bash-5.2/bin/bash",
		"/bin/bash-static",
	} {
		conn := base
		conn.BashPath = path
		if err := conn.Validate(); err != nil {
			t.Errorf("the ordinary path %q was refused: %v", path, err)
		}
	}

	for _, hostile := range []string{
		"bash",
		"../../bin/bash",
		"/bin/bash; touch /tmp/pwned",
		"/bin/bash && id",
		"/bin/bash $(id)",
		"/bin/bash`id`",
		"/bin/bash 'x'",
		"/bin/bash\nid",
		"/bin/bash|id",
		"/bin/bash >/tmp/x",
		"/bin/bash\t-x",
	} {
		conn := base
		conn.BashPath = hostile
		err := conn.Validate()
		if err == nil {
			t.Errorf("%q was accepted as a bash path, and it becomes part of a remote command string", hostile)

			continue
		}
		if !strings.Contains(err.Error(), "fixed remote command") {
			t.Errorf("the refusal for %q does not explain why: %v", hostile, err)
		}
	}
}

// TestTheRemoteCommandIsFixedAndCarriesNothingElse pins the one string this
// package ever sends as a command. Everything about the envelope depends on
// it: no PTY is not visible here, but --noprofile --norc, -s, and the
// absence of any injected shell option are.
func TestTheRemoteCommandIsFixedAndCarriesNothingElse(t *testing.T) {
	t.Parallel()

	c := &Client{conn: Connection{Ref: "db-hooks", BashPath: "/usr/local/bin/bash", Source: transport.Source{User: "hookuser"}}}

	if got, want := c.remoteCommand("backupd-exec-r1-0001"), "exec /usr/local/bin/bash --noprofile --norc -s backupd-exec-r1-0001"; got != want {
		t.Errorf("remoteCommand = %q, want %q", got, want)
	}
	if got, want := c.remoteCommand(""), "exec /usr/local/bin/bash --noprofile --norc -s"; got != want {
		t.Errorf("the internal-session command = %q, want %q (no token, or the reaper matches itself)", got, want)
	}
	for _, forbidden := range []string{"-e", "-u", "pipefail", "-x", "-i", "-l", "--login", "export "} {
		if strings.Contains(c.remoteCommand("tok"), forbidden) {
			t.Errorf("the remote command carries %q", forbidden)
		}
	}
}

func TestARequestTokenCannotCarryShellSyntax(t *testing.T) {
	t.Parallel()

	sink := &countingSink{}
	for _, hostile := range []string{
		"tok; touch /tmp/pwned",
		"tok$(id)",
		"tok`id`",
		"tok x",
		"tok\nid",
		"",
		strings.Repeat("t", 121),
	} {
		req := Request{Token: hostile, Sink: sink, Script: []byte("true\n")}
		if err := req.validate(); err == nil {
			t.Errorf("the token %q was accepted, and a token becomes part of the remote command line", hostile)
		}
	}

	req := Request{Token: "backupd-exec-run1-0003.quiesce", Sink: sink}
	if err := req.validate(); err != nil {
		t.Errorf("an ordinary token was refused: %v", err)
	}
}

func TestARequestWithNowhereToPutOutputIsRefused(t *testing.T) {
	t.Parallel()

	req := Request{Token: "tok", Script: []byte("true\n")}
	if err := req.validate(); err == nil {
		t.Fatal("a request with no sink was accepted; its output would have gone nowhere while the step reported as captured")
	}
}
