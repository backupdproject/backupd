package rclone

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/retnd/retnd/core/internal/transport"
)

// #810's exec client needs an ssh.Signer and a known_hosts path for a
// Source, and the only thing these tests are really about is that it gets
// them through the SAME custody rules a transfer over that Source goes
// through. Every case below has a matching refusal in sftpConfig's own
// tests; what is asserted here is that the exported pair does not have a
// weaker version of it.

func execSource(t *testing.T, mutate func(*transport.Source)) transport.Source {
	t.Helper()

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, mustUnencryptedKeyPEM(t), 0o600); err != nil {
		t.Fatalf("writing key: %v", err)
	}
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte("example.com ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatalf("writing known_hosts: %v", err)
	}

	src := transport.Source{
		ID:         "exec-source",
		Type:       "sftp",
		Host:       "example.com",
		User:       "hookuser",
		KeyFile:    keyPath,
		KnownHosts: knownHosts,
	}
	if mutate != nil {
		mutate(&src)
	}

	return src
}

func TestSourceSignerResolvesEveryConfiguredKeyLocation(t *testing.T) {
	pem := mustUnencryptedKeyPEM(t)

	t.Run("key_file", func(t *testing.T) {
		t.Parallel()
		signer, err := SourceSigner(execSource(t, nil))
		if err != nil {
			t.Fatalf("SourceSigner: %v", err)
		}
		if signer.PublicKey() == nil {
			t.Error("the signer has no public half")
		}
	})

	t.Run("key_env", func(t *testing.T) {
		const name = "RETND_TEST_EXEC_KEY"
		t.Setenv(name, string(pem))
		signer, err := SourceSigner(execSource(t, func(s *transport.Source) {
			s.KeyFile = ""
			s.KeyEnv = name
		}))
		if err != nil {
			t.Fatalf("SourceSigner: %v", err)
		}
		if signer.PublicKey() == nil {
			t.Error("the signer has no public half")
		}
	})

	t.Run("key_command", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "key.pem")
		if err := os.WriteFile(path, pem, 0o600); err != nil {
			t.Fatalf("writing key: %v", err)
		}
		signer, err := SourceSigner(execSource(t, func(s *transport.Source) {
			s.KeyFile = ""
			s.KeyCommand = []string{"cat", path}
		}))
		if err != nil {
			t.Fatalf("SourceSigner: %v", err)
		}
		if signer.PublicKey() == nil {
			t.Error("the signer has no public half")
		}
	})
}

// TestSourceSignerKeepsTheKeyFileCustodyRules is the one that would notice
// the exec path quietly skipping #293/#311: the mode and directory-chain
// checks are the reason a key this deployment no longer owns exclusively
// is refused rather than used.
func TestSourceSignerKeepsTheKeyFileCustodyRules(t *testing.T) {
	t.Parallel()

	src := execSource(t, nil)
	if err := os.Chmod(src.KeyFile, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := SourceSigner(src)
	if err == nil {
		t.Fatal("a world-readable key file was accepted for exec, while a transfer over the same source refuses it")
	}
	if !strings.Contains(err.Error(), "0600") && !strings.Contains(err.Error(), "mode") {
		t.Errorf("the refusal does not name the permission problem: %v", err)
	}
}

func TestSourceSignerRefusalsMatchTheTransferRules(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*transport.Source)
		mustSay string
	}{{
		name:    "no key source at all",
		mutate:  func(s *transport.Source) { s.KeyFile = "" },
		mustSay: "exactly one of key_file, key_env or key_command is required",
	}, {
		name:    "two key sources",
		mutate:  func(s *transport.Source) { s.KeyEnv = "SOMETHING" },
		mustSay: "not more than one",
	}, {
		name:    "two passphrase sources",
		mutate:  func(s *transport.Source) { s.PassphraseEnv = "A"; s.PassphraseFile = "/tmp/b" },
		mustSay: "may be set",
	}, {
		name:    "a key file that is not there",
		mutate:  func(s *transport.Source) { s.KeyFile = "/nonexistent/key" },
		mustSay: "not accessible",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := SourceSigner(execSource(t, c.mutate))
			if err == nil {
				t.Fatal("this source was accepted")
			}
			if !strings.Contains(err.Error(), c.mustSay) {
				t.Errorf("the refusal does not say %q: %v", c.mustSay, err)
			}
		})
	}
}

// TestSourceSignerUsesTheConfiguredPassphrase is about the diagnostic as
// much as the mechanism: a wrong passphrase has to be reported as a
// passphrase problem, because the alternative is "permission denied
// (publickey)" from the far side, which points an operator at the server.
func TestSourceSignerUsesTheConfiguredPassphrase(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "encrypted_key")
	if err := os.WriteFile(keyPath, mustEncryptedKeyPEM(t, false), 0o600); err != nil {
		t.Fatalf("writing key: %v", err)
	}
	passPath := filepath.Join(dir, "passphrase")
	if err := os.WriteFile(passPath, []byte(testEncryptedKeyPassphrase), 0o600); err != nil {
		t.Fatalf("writing passphrase: %v", err)
	}

	base := func(s *transport.Source) {
		s.KeyFile = keyPath
		s.PassphraseFile = passPath
	}

	signer, err := SourceSigner(execSource(t, base))
	if err != nil {
		t.Fatalf("SourceSigner with the right passphrase: %v", err)
	}
	if signer.PublicKey() == nil {
		t.Error("the signer has no public half")
	}

	wrongPath := filepath.Join(dir, "wrong")
	if err := os.WriteFile(wrongPath, []byte("not-the-passphrase"), 0o600); err != nil {
		t.Fatalf("writing passphrase: %v", err)
	}
	_, err = SourceSigner(execSource(t, func(s *transport.Source) {
		base(s)
		s.PassphraseFile = wrongPath
	}))
	if err == nil {
		t.Fatal("a wrong passphrase was accepted")
	}
	if !strings.Contains(err.Error(), "passphrase") {
		t.Errorf("the refusal does not name the passphrase, so an operator would go and look at the server: %v", err)
	}
}

func TestSourceSignerRefusesAPassphraseProtectedKeyWithNoPassphraseSource(t *testing.T) {
	t.Parallel()

	keyPath := filepath.Join(t.TempDir(), "encrypted_key")
	if err := os.WriteFile(keyPath, mustEncryptedKeyPEM(t, false), 0o600); err != nil {
		t.Fatalf("writing key: %v", err)
	}

	_, err := SourceSigner(execSource(t, func(s *transport.Source) { s.KeyFile = keyPath }))
	if err == nil {
		t.Fatal("a passphrase-protected key with no configured passphrase was accepted")
	}
	if !strings.Contains(err.Error(), "passphrase") {
		t.Errorf("the refusal does not name the missing passphrase source: %v", err)
	}
}

func TestSourceKnownHostsFileHoldsHostKeyVerificationOpen(t *testing.T) {
	t.Parallel()

	t.Run("a configured file resolves", func(t *testing.T) {
		t.Parallel()
		src := execSource(t, nil)
		got, err := SourceKnownHostsFile(src)
		if err != nil {
			t.Fatalf("SourceKnownHostsFile: %v", err)
		}
		if got != src.KnownHosts {
			t.Errorf("path = %q, want %q", got, src.KnownHosts)
		}
	})

	cases := []struct {
		name    string
		mutate  func(*transport.Source)
		mustSay string
	}{{
		name:    "nothing configured",
		mutate:  func(s *transport.Source) { s.KnownHosts = "" },
		mustSay: "known_hosts is required",
	}, {
		// rclone's own escape hatch: the literal "none" disables host-key
		// checking. An exec client that honoured it would run a hook on
		// whatever host answered.
		name:    "rclone's none escape hatch",
		mutate:  func(s *transport.Source) { s.KnownHosts = "none" },
		mustSay: "disables host-key verification",
	}, {
		name:    "a capitalised none",
		mutate:  func(s *transport.Source) { s.KnownHosts = " NONE " },
		mustSay: "disables host-key verification",
	}, {
		name:    "a path that is not there",
		mutate:  func(s *transport.Source) { s.KnownHosts = "/nonexistent/known_hosts" },
		mustSay: "not accessible",
	}, {
		name:    "a directory",
		mutate:  func(s *transport.Source) { s.KnownHosts = os.TempDir() },
		mustSay: "is a directory",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := SourceKnownHostsFile(execSource(t, c.mutate)); err == nil {
				t.Fatal("this known_hosts configuration was accepted")
			} else if !strings.Contains(err.Error(), c.mustSay) {
				t.Errorf("the refusal does not say %q: %v", c.mustSay, err)
			}
		})
	}
}
