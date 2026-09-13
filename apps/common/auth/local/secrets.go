package local

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Where the SMTP password lives, and why it does not live in the store
// file next to everything else.
//
// #830's secret-custody rule is the whole of this file's reason to exist:
// the SMTP password must never be persisted or logged in plaintext, must
// stay out of API responses, and must not be reachable by anything that
// can read the administrator record. So the administrator record does not
// hold it. It holds a reference - SMTPRecord.PasswordRef, an opaque name -
// and the material sits alone in a mode-0600 file of its own, in a 0700
// directory beside the store.
//
// That is deliberately the same custody shape core/service already uses
// for imported S3 credentials (MediumCredentialRef in
// core/service/mediumcredentials.go): material arrives once, lands in a
// file nothing else shares, and every surface afterwards carries only the
// reference. Copying it rather than inventing a second model is the point;
// two answers to "where does a secret go" is how one of them ends up
// wrong. core/internal/secretref is the resolver for the OPERATOR-declared
// form of the same idea (file/env/command), and is unavailable here for a
// mechanical reason as well as a design one: it is an internal package of
// a different module, so apps/common cannot import it at all.
//
// What this file does NOT do is encrypt. There is no key this process
// could hold that an attacker able to read a 0600 file in the state
// directory could not also read - the store file beside it holds the
// password hash, and the state directory holds everything else this
// product trusts - so encryption here would be obfuscation with a key
// management story attached. The boundary is the filesystem's, and it is
// the same boundary CreateAdmin's own doc already argues is equivalent to
// full trust in this product's threat model.

// smtpSecretsDirName is the directory, beside the store file, that holds
// one file per SMTP password this deployment has been given.
//
// Beside the store rather than under any backup root, for the reason
// core/service's mediumCredentialsDirName states: a backup root is what a
// NAS deployment exports over SMB, and a secret in it is a secret on the
// LAN. The store's own directory is state, is not exported, and is
// already where the password hash lives.
const smtpSecretsDirName = "smtp_secrets"

// errNoSecretRef is returned when a reference names nothing this
// deployment ever wrote. It is unexported because no caller outside this
// package can act on it differently: a missing password file means the
// SMTP configuration is unusable, and every caller reports that the same
// way.
var errNoSecretRef = errors.New("local: no such SMTP password reference")

// secretVault stores and resolves SMTP passwords by reference.
//
// It holds a directory path and nothing else: no cache, no in-memory copy
// of any secret, and no lifetime longer than the call that asked for one.
// A resolved password exists as a Go string for the duration of one send
// and is then unreferenced, which is as far as this language lets a
// caller go.
type secretVault struct{ dir string }

// newSecretVault returns the vault for a store at storePath. The
// directory is not created until something is actually stored, so a
// deployment that never configures SMTP never grows an empty secrets
// directory.
func newSecretVault(storePath string) *secretVault {
	return &secretVault{dir: filepath.Join(filepath.Dir(storePath), smtpSecretsDirName)}
}

// put writes password as a new secret and returns its reference.
//
// A fresh reference every time, rather than overwriting the previous
// file: a password being replaced is a different secret from the one it
// replaces, and reusing the name would mean a partially written file
// could be read as the old password. The caller swaps the reference in
// the store only once this has returned, and deletes the old one only
// after that write succeeded (see Service.replaceSMTPPassword).
func (v *secretVault) put(password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("local: refusing to store an empty SMTP password")
	}
	if err := os.MkdirAll(v.dir, 0o700); err != nil {
		return "", fmt.Errorf("local: create SMTP secrets directory: %w", err)
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("local: generate SMTP password reference: %w", err)
	}
	ref := hex.EncodeToString(raw)
	// 0600 at creation rather than fixed afterwards, and asserted with an
	// explicit Chmod for the reason core/service's writeMediumCredentials
	// states: a umask can only remove bits from the mode WriteFile asks
	// for, and a file that came out 0400 would make every later write
	// fail in a way nothing else would explain.
	path := filepath.Join(v.dir, ref)
	if err := os.WriteFile(path, []byte(password), 0o600); err != nil {
		// The path, never the material. A write failure is a fact about
		// this host's filesystem.
		return "", fmt.Errorf("local: persist SMTP password: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("local: set permissions on SMTP password: %w", err)
	}
	return ref, nil
}

// get resolves ref to the password it names.
//
// ref is checked for path separators before it is joined, exactly as
// core/service's resolveMediumCredentialsFileIn checks its own: this
// package only ever mints hex, so a reference carrying a separator is
// never one it issued and is refused outright rather than allowed to
// resolve somewhere else on this host. The refusal never quotes the
// reference back.
func (v *secretVault) get(ref string) (string, error) {
	if ref == "" {
		return "", errNoSecretRef
	}
	if strings.ContainsAny(ref, "/\\") || ref == "." || ref == ".." {
		return "", fmt.Errorf("%w: that is not a reference this deployment issued", errNoSecretRef)
	}
	b, err := os.ReadFile(filepath.Join(v.dir, ref))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errNoSecretRef
		}
		return "", fmt.Errorf("local: read SMTP password: %w", err)
	}
	// No trimming beyond a single trailing newline: put writes the
	// password verbatim, so anything else here would silently change a
	// password whose last character is a space into one that is not.
	return strings.TrimSuffix(string(b), "\n"), nil
}

// remove deletes the secret ref names, best effort.
//
// Best effort because it is only ever called AFTER the store no longer
// references it: a file that outlives its reference is 0600 garbage in a
// 0700 directory, while a failed removal that propagated would turn a
// successful settings change into an error the operator cannot act on.
func (v *secretVault) remove(ref string) {
	if ref == "" || strings.ContainsAny(ref, "/\\") || ref == "." || ref == ".." {
		return
	}
	_ = os.Remove(filepath.Join(v.dir, ref))
}
