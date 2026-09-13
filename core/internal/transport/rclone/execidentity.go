package rclone

import (
	"fmt"
	"os"
	"strings"

	"github.com/rclone/rclone/lib/env"
	"golang.org/x/crypto/ssh"

	"github.com/backupdproject/backupd/core/internal/obs"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// The SSH identity of a transport.Source, resolved once and exported for a
// second consumer: #810's remote execution client.
//
// # Why this is here and not in the package that needs it
//
// Because key custody has one owner. sftpConfig above decides which of
// key_file, key_env and key_command is in play, refuses more than one,
// refuses none, resolves the passphrase, checks the key file's mode and
// its whole directory chain, and applies #298's at-rest decryption. Every
// one of those is a security control with an argument behind it, written
// down in this file and in keysource.go, keyencryption.go and
// passphrase.go. A second package that needed an SSH identity and built
// its own would either re-derive those decisions or, far more likely,
// quietly skip some.
//
// So the switch is extracted (resolveSourceCredential) and both callers go
// through it: the SFTP transport, which hands the result to rclone, and
// the exec client, which needs an ssh.Signer because it speaks SSH itself.
//
// # The one property the exec client cannot keep
//
// key_file's documented advantage is that this process never opens it:
// rclone's own sftp backend reads the file, so the key never enters this
// program's memory (see sftpConfig's comment). That is not available to an
// exec client. There is no external binary doing the SSH for it -- it IS
// the SSH client -- so a signer has to be built here, in this process,
// from bytes read here.
//
// This is stated rather than glossed because it is a real, if small,
// widening of where key material lives: a core dump or a debugger
// attached to this process while a remote hook is running can see the
// key, which was not true of an artifact transfer over the same
// credential. Everything else is unchanged: the mode and directory-chain
// checks still run, the at-rest decryption still runs, the passphrase
// still comes from the configured source, and nothing is written anywhere.
// A deployment that wants the narrower property for its exec connection
// has the same option it always had for the transfer one -- key_env or
// key_command -- neither of which ever had it either.

// sourceCredential is one Source's resolved SSH credential: exactly one of
// the two key locations, plus the passphrase if one is configured.
type sourceCredential struct {
	// keyFile is the configured spelling of the key file, set only when
	// the key is to be read from that file as-is. Empty when the material
	// is in pem instead.
	keyFile string

	// pem is the key material, set when the key came from key_env,
	// key_command, or a key_file this process had to decrypt (#298).
	pem obs.Secret

	// usingPEM says which of the two above is populated. It is explicit
	// rather than inferred from emptiness, because "" is a value a
	// resolver could conceivably return and an empty key must never be
	// read as "use the file".
	usingPEM bool

	passphrase    obs.Secret
	hasPassphrase bool
}

// resolveSourceCredential is the credential half of sftpConfig, extracted
// so the exec client reaches exactly the same decisions. See this file's
// comment for why that matters and ssh.go's for what each refusal is for.
func resolveSourceCredential(src transport.Source) (sourceCredential, error) {
	sourceCount := 0
	if src.KeyFile != "" {
		sourceCount++
	}
	if src.KeyEnv != "" {
		sourceCount++
	}
	if len(src.KeyCommand) > 0 {
		sourceCount++
	}
	switch {
	case sourceCount == 0:
		return sourceCredential{}, fmt.Errorf("source %q: exactly one of key_file, key_env or key_command is required for sftp (key-based authentication is mandatory, ssh-agent fallback and password login are not offered)", src.ID)
	case sourceCount > 1:
		return sourceCredential{}, fmt.Errorf("source %q: exactly one of key_file, key_env or key_command may be set for sftp, not more than one", src.ID)
	}

	passphraseSourceCount := 0
	if src.PassphraseFile != "" {
		passphraseSourceCount++
	}
	if src.PassphraseEnv != "" {
		passphraseSourceCount++
	}
	if len(src.PassphraseCommand) > 0 {
		passphraseSourceCount++
	}
	if passphraseSourceCount > 1 {
		return sourceCredential{}, fmt.Errorf("source %q: exactly one of key_passphrase_file, key_passphrase_env or key_passphrase_command may be set for sftp, not more than one", src.ID)
	}

	passphraseSecret, hasPassphrase, err := resolvePassphrase(src)
	if err != nil {
		return sourceCredential{}, fmt.Errorf("source %q: resolving the SSH key passphrase: %w", src.ID, err)
	}
	passphrase := ""
	if hasPassphrase {
		passphrase = passphraseSecret.Reveal()
	}

	cred := sourceCredential{passphrase: passphraseSecret, hasPassphrase: hasPassphrase}

	switch {
	case src.KeyFile != "":
		keyFilePath := env.ShellExpand(src.KeyFile)
		info, err := os.Stat(keyFilePath)
		if err != nil {
			return sourceCredential{}, fmt.Errorf("source %q: key_file %q is not accessible: %w", src.ID, src.KeyFile, err)
		}
		if err := checkKeyFileMode(src.ID, src.KeyFile, info); err != nil {
			return sourceCredential{}, err
		}
		if err := checkKeyDirChainMode(src.ID, src.KeyFile, keyFilePath); err != nil {
			return sourceCredential{}, err
		}

		secret, usingPEM, err := resolveKeyFileForSFTP(src)
		if err != nil {
			return sourceCredential{}, fmt.Errorf("source %q: %w", src.ID, err)
		}
		if usingPEM {
			cred.pem = secret
			cred.usingPEM = true

			return cred, nil
		}
		cred.keyFile = src.KeyFile
	case src.KeyEnv != "":
		secret, err := resolveKeyFromEnv(src.KeyEnv, passphrase)
		if err != nil {
			return sourceCredential{}, fmt.Errorf("source %q: resolving the SSH key from environment variable %q: %w", src.ID, src.KeyEnv, err)
		}
		cred.pem = secret
		cred.usingPEM = true
	case len(src.KeyCommand) > 0:
		secret, err := resolveKeyFromCommand(src.KeyCommand, passphrase)
		if err != nil {
			return sourceCredential{}, fmt.Errorf("source %q: resolving the SSH key from the configured command: %w", src.ID, err)
		}
		cred.pem = secret
		cred.usingPEM = true
	}

	return cred, nil
}

// SourceSigner resolves src's configured SSH credential into a signer, for
// a client that speaks SSH in this process rather than through rclone.
//
// It applies the identical custody rules an artifact transfer over the same
// Source applies, because it calls the identical code. What it adds is the
// last step rclone would otherwise have done: parsing the key, with the
// resolved passphrase when one is configured.
//
// A passphrase that does not decrypt the key is a refusal here rather than
// an authentication failure later. The difference is what an operator
// reads: "this passphrase does not open this key" points at the
// passphrase, and "permission denied (publickey)" points at the server.
func SourceSigner(src transport.Source) (ssh.Signer, error) {
	cred, err := resolveSourceCredential(src)
	if err != nil {
		return nil, err
	}

	var pem []byte
	switch {
	case cred.usingPEM:
		pem = []byte(cred.pem.Reveal())
	default:
		// The one property an exec client cannot keep; see this file's
		// comment. The mode and directory-chain checks have already run
		// inside resolveSourceCredential, so by here the file is one this
		// deployment still owns exclusively.
		path := env.ShellExpand(cred.keyFile)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("source %q: reading key_file %q: %w", src.ID, cred.keyFile, err)
		}
		pem = raw
	}
	defer zeroBytes(pem)

	if cred.hasPassphrase {
		signer, err := ssh.ParsePrivateKeyWithPassphrase(pem, []byte(cred.passphrase.Reveal()))
		if err != nil {
			return nil, fmt.Errorf("source %q: the configured passphrase does not open this SSH key: %w", src.ID, err)
		}

		return signer, nil
	}

	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		if strings.Contains(err.Error(), "passphrase") {
			return nil, fmt.Errorf("source %q: this SSH key is passphrase-protected and no key passphrase source is configured for it: %w", src.ID, err)
		}

		return nil, fmt.Errorf("source %q: this SSH key cannot be parsed: %w", src.ID, err)
	}

	return signer, nil
}

// SourceKnownHostsFile returns the known_hosts path src verifies host keys
// against, after applying the three refusals FR-6 requires and sftpConfig
// makes: it must be configured, it must not be rclone's "none" escape
// hatch, and it must be a readable file rather than a directory.
//
// It exists so a client that does its own host-key verification verifies
// against the same file, decided by the same rules, as a transfer over the
// same Source. A second reading of the same setting is how one of two
// paths ends up trusting a host the other refuses.
func SourceKnownHostsFile(src transport.Source) (string, error) {
	if src.KnownHosts == "" {
		return "", fmt.Errorf("source %q: known_hosts is required for sftp", src.ID)
	}
	if strings.EqualFold(strings.TrimSpace(src.KnownHosts), "none") {
		return "", fmt.Errorf("source %q: known_hosts value %q disables host-key verification, which this adapter refuses to allow", src.ID, src.KnownHosts)
	}

	path := env.ShellExpand(src.KnownHosts)
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("source %q: known_hosts %q is not accessible: %w", src.ID, src.KnownHosts, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("source %q: known_hosts %q is a directory, not a file", src.ID, src.KnownHosts)
	}

	return path, nil
}
