package config

import (
	"strings"
	"testing"
)

// The execution connection is a DIFFERENT connection from the transfer
// connection, and #810's whole premise is that it has to be expressible as
// one: a hardened backup source legitimately uses an SFTP-only or
// forced-command account, and the credential that pulls artifacts off that
// host cannot be assumed to grant shell exec on it.
//
// So a deployment may declare execution connections of their own, carrying
// the same controls a transfer remote carries -- host, port, user,
// credential reference, known_hosts, connection ceiling -- and a backup
// set's remote_exec_connection_ref names one.

func execConnectionYAML(id, extra string) string {
	return `
poll_interval: 15m
state:
  database: /var/lib/backupd/state.db
workflows:
  root: /workflows
  global:
    before_dir: before
  exec_connections:
    - id: ` + id + `
      remote:
        type: sftp
        host: db.example.com
        user: backupd-hooks
        known_hosts: /etc/backupd/known_hosts
        key:
          file: /etc/backupd/hooks.key
` + extra + `
sources:
  - id: production
    backup_sets:
      - id: db
        remote:
          type: sftp
          host: db.example.com
          user: backupd-transfer
          known_hosts: /etc/backupd/known_hosts
          key:
            file: /etc/backupd/transfer.key
        remote_path: /var/backups
        local_path: /srv/backups/db
        completion:
          strategy: rename
        stale_after: 30h
        workflow:
          remote_exec_connection_ref: ` + id + `
`
}

func TestAnExecConnectionIsConfigurableSeparatelyFromTheTransferRemote(t *testing.T) {
	t.Parallel()

	cfg := loadYAML(t, execConnectionYAML("db-hooks", ""))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a deployment declaring a separate execution connection was refused: %v", err)
	}

	got := cfg.Workflows.ExecConnections
	if len(got) != 1 {
		t.Fatalf("exec_connections = %+v, want one entry", got)
	}
	if got[0].Name != "db-hooks" {
		t.Errorf("id = %q", got[0].Name)
	}
	if got[0].Remote.User != "backupd-hooks" {
		t.Errorf("user = %q; the execution credential is not the transfer credential", got[0].Remote.User)
	}

	// The reference resolves, and it resolves to the EXEC connection
	// rather than to the set's own source.
	conn, ok := cfg.WorkflowExecConnection("db-hooks")
	if !ok {
		t.Fatal("WorkflowExecConnection did not find the connection the config declares")
	}
	if conn.Remote.User != "backupd-hooks" {
		t.Errorf("resolved user = %q", conn.Remote.User)
	}
	if set := cfg.Sources[0].BackupSets[0]; set.Remote.User == conn.Remote.User {
		t.Error("the exec connection and the transfer remote resolved to the same credential")
	}
}

func TestExecConnectionRefusals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		yaml    string
		mustSay string
	}{{
		name:    "a connection with no id",
		yaml:    execConnectionYAML(`""`, ""),
		mustSay: "id is required",
	}, {
		name: "two connections with the same id",
		yaml: execConnectionYAML("db-hooks", `    - id: db-hooks
      remote:
        type: sftp
        host: other.example.com
        user: someone
        known_hosts: /etc/backupd/known_hosts
        key:
          file: /etc/backupd/other.key`),
		mustSay: "declared twice",
	}, {
		name:    "an id that could be read as a backup set id",
		yaml:    execConnectionYAML("production/db", ""),
		mustSay: `must not contain "/"`,
	}, {
		// An execution connection reached over "local" would be this
		// daemon's own host, which is what a NAME.local.sh already is.
		name: "a local execution connection",
		yaml: strings.Replace(execConnectionYAML("db-hooks", ""),
			"        type: sftp\n        host: db.example.com\n        user: backupd-hooks\n        known_hosts: /etc/backupd/known_hosts\n        key:\n          file: /etc/backupd/hooks.key",
			"        type: local", 1),
		mustSay: "must be \"sftp\"",
	}, {
		name: "host-key verification turned off",
		yaml: strings.Replace(execConnectionYAML("db-hooks", ""),
			"        known_hosts: /etc/backupd/known_hosts\n        key:\n          file: /etc/backupd/hooks.key",
			"        known_hosts: none\n        key:\n          file: /etc/backupd/hooks.key", 1),
		mustSay: "host-key verification",
	}, {
		name: "no key at all",
		yaml: strings.Replace(execConnectionYAML("db-hooks", ""),
			"        key:\n          file: /etc/backupd/hooks.key\n", "", 1),
		mustSay: "key",
	}, {
		name: "a reference naming nothing at all",
		yaml: strings.Replace(execConnectionYAML("db-hooks", ""),
			"remote_exec_connection_ref: db-hooks", "remote_exec_connection_ref: typo-hooks", 1),
		mustSay: "names no execution connection",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cfg := loadYAML(t, c.yaml)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("this configuration was accepted:\n%s", c.yaml)
			}
			if !strings.Contains(err.Error(), c.mustSay) {
				t.Errorf("the refusal does not say %q:\n%v", c.mustSay, err)
			}
		})
	}
}

// TestARefToTheSetsOwnSourceIsAccepted is the second half of #810's
// resolution rule: a reference may name the backup set's own source
// connection, spelled the way every other surface in this product spells a
// backup set ("source/set"). Whether that connection may actually run a
// hook is not a config question at all -- it is settled by a real exec
// capability preflight at run start -- and refusing it here would refuse
// the perfectly ordinary deployment where the source account does have a
// shell.
func TestARefToTheSetsOwnSourceIsAccepted(t *testing.T) {
	t.Parallel()

	yaml := strings.Replace(execConnectionYAML("db-hooks", ""),
		"remote_exec_connection_ref: db-hooks", "remote_exec_connection_ref: production/db", 1)
	cfg := loadYAML(t, yaml)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a reference to the set's own source connection was refused: %v", err)
	}
	if _, ok := cfg.WorkflowExecConnection("production/db"); ok {
		t.Error("a backup set id resolved as a declared execution connection; those are two different kinds of reference and only one of them is exec-capable by declaration")
	}
}

// TestExecConnectionsNeedAWorkflowRoot keeps the block from being
// configuration that does nothing, which is the rule workflows.environment
// already follows.
func TestExecConnectionsNeedAWorkflowRoot(t *testing.T) {
	t.Parallel()

	cfg := loadYAML(t, strings.Replace(execConnectionYAML("db-hooks", ""), "  root: /workflows\n", "", 1))
	err := cfg.Validate()
	if err == nil {
		t.Fatal("an execution connection with no workflow root at all was accepted, and nothing would ever run over it")
	}
	if !strings.Contains(err.Error(), "workflows.root") {
		t.Errorf("the refusal does not name the missing root: %v", err)
	}
}
