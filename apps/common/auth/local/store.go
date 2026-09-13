package local

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The one thing this package persists: a username and a password hash.
//
// Everything else it holds (sessions, bootstrap tokens, rate-limit
// counters) is deliberately in memory, so this file is the whole of the
// on-disk surface, and it is a single small JSON file rather than a
// database because one record does not justify one.
//
// Two properties are load-bearing. Writes go through a temp file and a
// rename, so a crash mid-write leaves the previous file intact rather than
// a truncated one that fails to parse on the next start and locks the
// operator out. And Enroll refuses when a record already exists, which is
// §49.1's single-shot rule enforced at the lowest level rather than only
// in the handler: the handler checks too, but the handler's check and its
// write are not atomic, so this is what actually decides a race.
//
// Store takes no OS-level lock of its own. It is the path-only primitive,
// and both callers that own a store for longer than one call (Service.New,
// CreateAdmin) take the lock in lock_unix.go around it.

// AdminRecord is the one persisted local-auth identity this package
// supports today (docs/EPIC-B-multi-nas.md §13.4's admin-only initial
// release): a username, an Argon2id password hash (password.go) and the
// address account recovery mails to. PasswordHash is never a plaintext
// password.
//
// RecoveryEmail is an ADDITIONAL field, not a replacement for Username
// (issue #830 is explicit about that): Username remains the login
// identity, and this address is used for recovery and notification only.
// It is validated as an address before it is ever persisted
// (apps/common/email.ValidateAddress) by every writer - handleEnroll,
// handleUpdateRecovery and CreateAdmin alike - so a record on disk always
// either carries a parseable address or carries none at all.
type AdminRecord struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`

	// RecoveryEmail is where a forgotten-password reset link is sent. It
	// is empty only for a record provisioned before #830 existed; every
	// path that writes one now requires it.
	RecoveryEmail string `json:"recovery_email,omitempty"`

	// RecoveryEmailConfirmedAt records when a message to RecoveryEmail
	// was last accepted by the operator's own SMTP server. It is the
	// SENDING half of the pair below: the mail server took the message.
	// A changed address is unconfirmed until its own send succeeds.
	RecoveryEmailConfirmedAt *time.Time `json:"recovery_email_confirmed_at,omitempty"`

	// RecoveryEmailVerifiedAt records when somebody actually opened the
	// verification link that message carried (#830 §8). It is the
	// RECEIVING half, and the two are deliberately separate facts: an
	// SMTP server accepting a message proves the endpoint works, and
	// nothing more - a typo'd-but-deliverable address (the neighbouring
	// domain, a colleague's mailbox) is accepted just as happily as the
	// right one. Only a redeemed link proves the operator can READ what
	// was sent, which is the whole of what account recovery depends on.
	//
	// nil means unverified, and an unverified record with a
	// VerificationDeadline in the past is deleted (verify.go's reaper).
	RecoveryEmailVerifiedAt *time.Time `json:"recovery_email_verified_at,omitempty"`

	// VerificationDeadline is when an unverified record lapses: the
	// moment after which the reaper deletes this administrator and
	// reopens enrollment (#830 §9). It is computed once, at creation
	// (verificationDeadline in verify.go), and never moved afterwards -
	// a deadline a resend could push out would be no deadline at all.
	//
	// nil means "nothing to verify, nothing to lapse", and that is a
	// real state rather than a missing value: a record provisioned by
	// `auth create-admin` with no SMTP endpoint (provision.go) never had
	// a verification message sent, so there is no link anybody could
	// open, and deleting the only administrator of an unattended
	// deployment 30 minutes after provisioning it would be the worst
	// possible reading of this feature. A record written before #830
	// existed has none either, and must equally never be reaped.
	VerificationDeadline *time.Time `json:"verification_deadline,omitempty"`

	// VerificationTokenHash and VerificationTokenExpiresAt are the
	// PENDING verification challenge: the SHA-256 of the token the last
	// verification message carried, and when that token stops being
	// accepted. Both are cleared the moment the link is redeemed, which
	// is what makes it single-use.
	//
	// The HASH and never the token: a store file an operator can read is
	// not a place to keep a live credential, and a verification link is
	// one. POST /verify-email hashes what it was given and compares in
	// constant time (verify.go), which needs nothing more than this.
	//
	// It is PERSISTED, unlike the bootstrap and password-reset tokens
	// this package keeps in memory, and the difference is forced rather
	// than stylistic. Those two are recovered by asking again - a
	// restart reprints the enrollment token, and forgot-password mails a
	// second link. This one is the only thing standing between a
	// provisional account and the reaper deleting it, so a token that
	// died with the process would mean a restart inside the verification
	// window guaranteed the loss of the account. It also has to cross a
	// process boundary by construction: `auth create-admin`
	// (provision.go) mints the challenge in a short-lived CLI process and
	// the server that redeems it starts afterwards.
	VerificationTokenHash      string     `json:"verification_token_hash,omitempty"`
	VerificationTokenExpiresAt *time.Time `json:"verification_token_expires_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// SMTPRecord is the persisted half of one SMTP submission endpoint: every
// field email.Config has EXCEPT the password, which is held as an opaque
// reference to a 0600 file this package wrote (secrets.go).
//
// That split is the whole point of this type existing separately from
// email.Config. The config struct is what a send needs in memory for as
// long as one message takes; this is what is allowed to survive on disk,
// and a plaintext password is not on the list. It is the same custody
// shape core/service's MediumCredentialRef already uses for S3 keys:
// material arrives once, lands in a mode-0600 file of its own, and
// everything afterwards carries the reference.
type SMTPRecord struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Security string `json:"security"`
	Username string `json:"username"`

	// PasswordRef names the file holding the SMTP password, relative to
	// the store's own secrets directory. It is empty when the endpoint
	// needs no authentication at all (a relay on the same host), which is
	// a legitimate configuration rather than a missing password.
	PasswordRef string `json:"password_ref,omitempty"`

	From string `json:"from"`
}

// storeFile is the on-disk shape Store persists. Enrollment is closed
// while Admin is non-nil (§49.1: "single-shot and irreversible") and the
// one thing that can ever set it back to nil is DeleteAdmin below -
// reaping a provisional administrator whose recovery address was never
// verified (#830 §9), which is not a reopening of a finished enrollment
// but the abandonment of an unfinished one.
//
// SMTP sits BESIDE Admin rather than inside it because it is a property
// of the deployment rather than of the identity: it is what the runtime
// uses to reach a mail server, it is editable after enrollment (Settings,
// #830), and keeping it out of AdminRecord means SetSMTP can never
// accidentally be the call that creates a partial administrator.
type storeFile struct {
	Admin *AdminRecord `json:"admin"`
	SMTP  *SMTPRecord  `json:"smtp,omitempty"`
}

// ErrAlreadyEnrolled is returned by Store.Enroll when an administrator
// record already exists.
var ErrAlreadyEnrolled = errors.New("local: an administrator account already exists")

// Store persists exactly one AdminRecord to a JSON file at path, guarded
// by an in-process mutex (this package assumes a single process owns
// path; the generic host's serve command is exactly that) and made
// durable one write at a time via write-temp-then-rename, so a crash
// mid-write can never leave a half-written file for the next start to
// trip over.
//
// "A single process owns path" is no longer just an assumption: New and
// CreateAdmin (service.go/provision.go) both take path's own exclusive
// advisory lock (lock_unix.go/lock_other.go) before either constructs a
// Store or writes through one, so a second process reaching for the same
// path - a duplicate `serve`, or `create-admin` racing a live one -
// is refused with ErrStoreLocked rather than racing this Store's own
// read-modify-write cycle. Store itself still does not take that lock;
// it is deliberately the lower-level, path-only primitive both callers
// build on.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore returns a Store backed by the JSON file at path. path's parent
// directory is created (mode 0700) on first write if it does not already
// exist; nothing is written or read until Admin or Enroll is called.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// load reads the file, treating "not there yet" as an empty store rather
// than an error: a deployment that has never enrolled has no file, and
// that is the normal first-start state, not a fault. A parse failure is
// different and is reported, because a file that exists and cannot be read
// means something wrote garbage over an administrator record and carrying
// on would quietly reopen enrollment.
func (s *Store) load() (storeFile, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return storeFile{}, nil
	}
	if err != nil {
		return storeFile{}, fmt.Errorf("local: read store %s: %w", s.path, err)
	}
	var f storeFile
	if err := json.Unmarshal(b, &f); err != nil {
		return storeFile{}, fmt.Errorf("local: parse store %s: %w", s.path, err)
	}
	return f, nil
}

// save replaces the file's whole contents. Callers hold s.mu and have
// already merged whatever they wanted to change into f, so there is no
// partial-update path here to get wrong.
func (s *Store) save(f storeFile) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("local: create store directory: %w", err)
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("local: encode store: %w", err)
	}
	// write-temp-then-rename: os.Rename is atomic on the same filesystem
	// (true of every mount this file is expected to live on: a bind-mounted
	// STATE_DIR volume, or a plain local directory in tests), so a reader
	// never observes a partially written file, and a crash between the
	// WriteFile and the Rename leaves the ORIGINAL file untouched rather
	// than a corrupted one.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("local: write store: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("local: commit store: %w", err)
	}
	return nil
}

// Admin returns the persisted administrator record, or nil if enrollment
// has not happened yet.
func (s *Store) Admin() (*AdminRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	return f.Admin, nil
}

// ErrNotEnrolled is returned by Store.SetPassword when no administrator
// exists yet - rotating a password before enrollment is meaningless, and
// this is the one method in this package that could otherwise silently
// create a partial admin record.
var ErrNotEnrolled = errors.New("local: no administrator account exists yet")

// SetPassword replaces the persisted administrator's password hash with
// newHash, leaving Username and CreatedAt untouched. It fails with
// ErrNotEnrolled if no administrator has enrolled yet - password rotation
// is meaningless before enrollment, and this is the one method in this
// package that could otherwise silently create a partial admin record.
func (s *Store) SetPassword(newHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	if f.Admin == nil {
		return ErrNotEnrolled
	}
	f.Admin.PasswordHash = newHash
	return s.save(f)
}

// Enroll persists admin as this store's one administrator record, and
// smtp (when non-nil) as the deployment's SMTP configuration, in ONE
// write. It fails with ErrAlreadyEnrolled if a record already exists:
// enrollment is single-shot and irreversible (§49.1), and this is the one
// method in this package that could otherwise silently overwrite an
// existing administrator.
//
// The two travel together rather than through two calls because #830
// makes a working SMTP endpoint part of what enrollment IS: an
// administrator persisted without the SMTP configuration that was just
// proved to work would be an account that cannot be recovered, and an
// SMTP configuration persisted without the administrator would leave a
// store that still looks unenrolled while holding a secret reference.
// One save, one rename, no intermediate state for either.
func (s *Store) Enroll(admin AdminRecord, smtp *SMTPRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	if f.Admin != nil {
		return ErrAlreadyEnrolled
	}
	f.Admin = &admin
	if smtp != nil {
		f.SMTP = smtp
	}
	return s.save(f)
}

// SMTP returns the persisted SMTP configuration, or nil when this
// deployment has never had one. The record it returns carries
// SMTPRecord.PasswordRef, never a password: resolving that reference is
// secrets.go's job and happens once per send.
func (s *Store) SMTP() (*SMTPRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	return f.SMTP, nil
}

// SetSMTP replaces the persisted SMTP configuration. It fails with
// ErrNotEnrolled before enrollment for SetPassword's reason: an SMTP
// endpoint on a store with no administrator belongs to nobody, and
// accepting one would make this the second method able to write a store
// that never went through Enroll.
func (s *Store) SetSMTP(rec SMTPRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	if f.Admin == nil {
		return ErrNotEnrolled
	}
	f.SMTP = &rec
	return s.save(f)
}

// RecoveryEmailState is the whole of what a writer says about the
// administrator's recovery address in one go: the address, the two
// proofs about it, and the verification challenge outstanding against
// it.
//
// It is one struct rather than five parameters because the five are one
// fact and a partial write of them is always a lie. An address changed
// without clearing its proofs claims a message was delivered somewhere
// nothing was sent; an address changed without replacing the challenge
// leaves the OLD address's live token able to verify the NEW one, which
// is a verification bypass rather than a cosmetic inconsistency. Store
// writes one file at a time, so making this one call is also what makes
// it one rename with no intermediate state on disk.
type RecoveryEmailState struct {
	Address string

	// ConfirmedAt is when a message to Address was last accepted by the
	// operator's SMTP server; VerifiedAt is when the link it carried was
	// opened. nil for either means "not proved".
	ConfirmedAt *time.Time
	VerifiedAt  *time.Time

	// TokenHash/TokenExpiresAt are the pending challenge, empty/nil when
	// there is none outstanding (nothing sent, or already redeemed).
	TokenHash      string
	TokenExpiresAt *time.Time
}

// SetRecoveryEmail replaces the persisted administrator's whole recovery
// block with st in one write, leaving every other field - the password
// hash, the creation time, the verification DEADLINE - untouched.
//
// The deadline is deliberately not part of st: it is written once, when
// the record is created, and a setter able to move it would be a way to
// give a provisional account an unbounded reprieve (see
// AdminRecord.VerificationDeadline).
func (s *Store) SetRecoveryEmail(st RecoveryEmailState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	if f.Admin == nil {
		return ErrNotEnrolled
	}
	f.Admin.RecoveryEmail = st.Address
	f.Admin.RecoveryEmailConfirmedAt = st.ConfirmedAt
	f.Admin.RecoveryEmailVerifiedAt = st.VerifiedAt
	f.Admin.VerificationTokenHash = st.TokenHash
	f.Admin.VerificationTokenExpiresAt = st.TokenExpiresAt
	return s.save(f)
}

// MarkRecoveryEmailVerified records that the verification link mailed to
// the current recovery address was opened, at at, and spends the
// challenge that link carried (#830 §8). It reports whether it wrote:
// false means the outstanding challenge is no longer the one tokenHash
// names, and nothing was changed.
//
// The challenge is RE-CHECKED here, under this store's own mutex,
// against the hash the caller matched, and that re-check is the whole
// reason this method takes a hash at all. The handler's own
// challengeAccepts runs in a first lock acquisition and this write used
// to run in a second, and anything that replaced the challenge in
// between - PATCH /auth/recovery pointing the account at a NEW address,
// or /verify-email/resend minting a new link - would have been marked
// verified on the strength of the OLD address's token. Deciding and
// writing in one critical section is what closes that: a token that was
// valid when it was read and superseded before it was spent now spends
// nothing, and the handler answers VERIFY_TOKEN_INVALID.
//
// The comparison is constant-time over the hashes for challengeAccepts'
// reason, and the expiry is re-read here too: the two facts that made
// the token acceptable are both properties of the record, so both are
// re-established against the record rather than remembered from the
// read.
//
// Three fields in one write, and each one has to be in it.
//
// The challenge is cleared by the same save that records the proof,
// which IS what single-use means here: a replay of the same link finds
// no challenge to match rather than a second acceptance.
//
// The DEADLINE is cleared because it has been met. It described the
// window this provisional account had to prove itself, and that window
// closed by being satisfied; leaving it behind would arm the reaper
// against an account that passed, the moment anything later set
// RecoveryEmailVerifiedAt back to nil - which is exactly what changing
// the recovery address on the Settings page does (handleUpdateRecovery).
// An established administrator who edits their address gets an
// unverified address and a nudge to verify it, never a deleted account.
//
// It re-states the address deliberately not at all: a verification that
// could write one would be a second way to change it, and the address
// this proof belongs to is already on the record.
func (s *Store) MarkRecoveryEmailVerified(at time.Time, tokenHash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return false, err
	}
	if f.Admin == nil {
		return false, ErrNotEnrolled
	}
	if !acceptsTokenHash(f.Admin, tokenHash, at) {
		return false, nil
	}
	f.Admin.RecoveryEmailVerifiedAt = &at
	f.Admin.VerificationDeadline = nil
	f.Admin.VerificationTokenHash = ""
	f.Admin.VerificationTokenExpiresAt = nil
	if err := s.save(f); err != nil {
		return false, err
	}
	return true, nil
}

// DeleteUnverifiedAdmin removes the administrator record and the SMTP
// configuration that belonged to it in ONE write, and only if that
// record is STILL a provisional one whose verification deadline has
// passed as of now. It returns the SMTP record it removed (nil when
// there was none) so the caller can drop the secret file it referenced,
// and whether it deleted anything at all.
//
// The condition is re-established HERE, under this store's own mutex,
// rather than trusted from the caller's earlier read, and that is the
// whole difference between this method and the unconditional delete it
// replaces. The reaper reads the record, decides, and calls; a
// verification committing in between turns a record that was reapable
// into one that is not, and an unconditional delete would then remove a
// NOW-VERIFIED administrator whose owner had just been answered 204.
// Deciding and deleting in one critical section is what makes the
// reaper's answer true at the instant it acts, and it is the same guard
// doctrine Enroll and SetPassword already apply: the check that decides
// a race lives beside the write it protects, never in the handler.
//
// It is also why the CLOCK is a parameter. The decision is the injected
// clock's to make (verify.go's opening note), and passing the instant in
// keeps this method free of one while still letting it be the only
// place the comparison happens.
//
// This is the one method that reopens enrollment, and it exists for
// exactly one caller: verify.go's reaper, deleting a PROVISIONAL
// administrator whose recovery address was never verified before its
// deadline (#830 §9). §49.1's "single-shot and irreversible" is not
// weakened by it - the rule is that a bootstrap token may only ever
// create ONE account, and what this deletes is an account that, by its
// own unverified state, was never finished. What reopens afterwards is a
// deployment with no administrator at all, which is the state §49.1
// describes and which Service.New handles by minting a fresh token.
//
// The two go together for Enroll's reason in reverse: an SMTP endpoint
// left behind by a deleted administrator would be a secret reference
// belonging to nobody, sitting in a store that reads as unenrolled, and
// the next enrollment writes its own endpoint anyway. Returning it (and
// removing the file only once the store no longer points at it) keeps
// that cleanup in the caller, which is where the secret vault lives.
//
// Deleting nothing is not an error: a reaper racing a verification, or
// two reapers on one store, must both be able to run to completion.
func (s *Store) DeleteUnverifiedAdmin(now time.Time) (*SMTPRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return nil, false, err
	}
	if f.Admin == nil || f.Admin.RecoveryEmailVerifiedAt != nil || f.Admin.VerificationDeadline == nil {
		return nil, false, nil
	}
	if now.Before(*f.Admin.VerificationDeadline) {
		return nil, false, nil
	}
	removed := f.SMTP
	f.Admin = nil
	f.SMTP = nil
	if err := s.save(f); err != nil {
		return nil, false, err
	}
	return removed, true, nil
}
