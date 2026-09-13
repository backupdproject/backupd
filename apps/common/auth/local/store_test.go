package local

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The persistence rules, including the two the rest of this package leans
// on without re-checking.
//
// Enroll being single-shot is one: every caller assumes an existing
// administrator cannot be overwritten, and this is the only place that is
// actually proved. SetPassword preserving the username and creation time
// is the other, because it does a read-modify-write and the easy mistake is
// to write a fresh record with only the field that changed.
//
// The plaintext test reads the raw file rather than the parsed struct, on
// purpose. A struct-level assertion would pass even if the plaintext were
// sitting in a field nothing maps back, and what actually matters is what
// somebody who cats the file can see.

func TestStore_AdminIsNilBeforeAnyEnrollment(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	admin, err := store.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if admin != nil {
		t.Errorf("Admin() = %+v, want nil (no store file exists yet)", admin)
	}
}

func TestStore_EnrollPersistsAndAdminReturnsIt(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "nested", "auth.json"))
	record := AdminRecord{Username: "bm-admin", PasswordHash: "$argon2id$fake", CreatedAt: time.Now().UTC()}
	if err := store.Enroll(record, nil); err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	got, err := store.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if got == nil {
		t.Fatal("Admin() = nil after Enroll, want the enrolled record")
	}
	if got.Username != record.Username || got.PasswordHash != record.PasswordHash {
		t.Errorf("Admin() = %+v, want %+v", *got, record)
	}
}

func TestStore_EnrollTwiceFailsWithErrAlreadyEnrolled(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	first := AdminRecord{Username: "bm-admin", PasswordHash: "$argon2id$fake1"}
	if err := store.Enroll(first, nil); err != nil {
		t.Fatalf("first Enroll: %v", err)
	}

	second := AdminRecord{Username: "someone-else", PasswordHash: "$argon2id$fake2"}
	err := store.Enroll(second, nil)
	if !errors.Is(err, ErrAlreadyEnrolled) {
		t.Fatalf("second Enroll error = %v, want errors.Is(err, ErrAlreadyEnrolled)", err)
	}

	// The original administrator must survive the rejected second attempt
	// untouched (§49.1: enrollment is single-shot and irreversible).
	got, err := store.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if got.Username != first.Username {
		t.Errorf("Admin().Username = %q after a rejected second Enroll, want the original %q", got.Username, first.Username)
	}
}

func TestStore_PersistsAcrossANewStoreInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	first := NewStore(path)
	if err := first.Enroll(AdminRecord{Username: "bm-admin", PasswordHash: "$argon2id$fake"}, nil); err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	// A brand new Store value pointed at the same path (simulating a
	// process restart) must see the same administrator - enrollment
	// closing must not depend on any in-memory state.
	second := NewStore(path)
	admin, err := second.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if admin == nil || admin.Username != "bm-admin" {
		t.Errorf("Admin() after reopening the store = %+v, want the persisted administrator", admin)
	}
}

func TestStore_SetPasswordUpdatesHashAndPreservesUsernameAndCreatedAt(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	created := time.Now().UTC().Truncate(time.Second)
	if err := store.Enroll(AdminRecord{Username: "bm-admin", PasswordHash: "$argon2id$old", CreatedAt: created}, nil); err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	if err := store.SetPassword("$argon2id$new"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	got, err := store.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if got.PasswordHash != "$argon2id$new" {
		t.Errorf("Admin().PasswordHash = %q, want %q", got.PasswordHash, "$argon2id$new")
	}
	if got.Username != "bm-admin" {
		t.Errorf("Admin().Username = %q after SetPassword, want unchanged %q", got.Username, "bm-admin")
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("Admin().CreatedAt = %v after SetPassword, want unchanged %v", got.CreatedAt, created)
	}
}

// TestStore_SetPasswordFailsBeforeEnrollment guards against SetPassword
// ever silently creating a partial administrator record: rotation is
// meaningless before an administrator exists at all.
func TestStore_SetPasswordFailsBeforeEnrollment(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	err := store.SetPassword("$argon2id$new")
	if !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("SetPassword before enrollment error = %v, want errors.Is(err, ErrNotEnrolled)", err)
	}
}

func TestStore_SetPasswordPersistsAcrossANewStoreInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	first := NewStore(path)
	if err := first.Enroll(AdminRecord{Username: "bm-admin", PasswordHash: "$argon2id$old"}, nil); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if err := first.SetPassword("$argon2id$new"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	// A brand new Store value pointed at the same path (simulating a
	// process restart) must see the rotated hash, not the pre-rotation one.
	second := NewStore(path)
	admin, err := second.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if admin == nil || admin.PasswordHash != "$argon2id$new" {
		t.Errorf("Admin() after reopening the store = %+v, want PasswordHash %q", admin, "$argon2id$new")
	}
}

func TestStore_NeverWritesAPlaintextPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	store := NewStore(path)
	encoded, err := hashPassword("super-secret-plaintext-value")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if err := store.Enroll(AdminRecord{Username: "bm-admin", PasswordHash: encoded}, nil); err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), "super-secret-plaintext-value") {
		t.Errorf("store file at %s contains the plaintext password", path)
	}
}

// The two store guards #830's security review added, each written as the
// exact interleaving it exists to refuse. Both live here rather than in
// verify_test.go because both are decisions the STORE makes under its own
// mutex: a handler-level test can only ask for the two requests to race
// and hope, while these state the outcome the lock is there to guarantee.

// provisionalAdmin writes a record in the state the reaper and the
// verification flow both act on: an outstanding challenge, nothing
// verified yet, and a deadline to lapse at.
func provisionalAdmin(t *testing.T, store *Store, token string, created, deadline time.Time) {
	t.Helper()
	expires := created.Add(verifyTokenTTL)
	if err := store.Enroll(AdminRecord{
		Username:                   "bm-admin",
		PasswordHash:               "$argon2id$fake",
		RecoveryEmail:              "admin@example.test",
		RecoveryEmailConfirmedAt:   &created,
		VerificationDeadline:       &deadline,
		VerificationTokenHash:      hashVerificationToken(token),
		VerificationTokenExpiresAt: &expires,
		CreatedAt:                  created,
	}, &SMTPRecord{Host: "smtp.invalid.test", Port: 587, Security: "starttls", From: "backupd@example.test"}); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
}

// The cross-address verification bypass (#830 review, HIGH): the handler
// matches the token against the record it read, and ANY writer can
// replace that challenge before the mark is written - a PATCH /recovery
// pointing the account at an attacker's address, or a resend. Marking
// unconditionally would report the NEW address as verified on the
// strength of a click on the OLD one, which is the whole verification
// property inverted.
func TestStore_MarkRecoveryEmailVerifiedRefusesAChallengeThatWasSuperseded(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	created := time.Now().UTC()
	provisionalAdmin(t, store, "the-mailed-token", created, created.Add(minVerificationWindow))
	mailed := hashVerificationToken("the-mailed-token")

	// The interleave: between the handler matching `mailed` and the
	// write below, the address is repointed and a new challenge minted.
	replacement := created.Add(verifyTokenTTL)
	if err := store.SetRecoveryEmail(RecoveryEmailState{
		Address:        "attacker@example.invalid",
		TokenHash:      hashVerificationToken("a-different-token"),
		TokenExpiresAt: &replacement,
	}); err != nil {
		t.Fatalf("SetRecoveryEmail: %v", err)
	}

	verified, err := store.MarkRecoveryEmailVerified(created, mailed)
	if err != nil {
		t.Fatalf("MarkRecoveryEmailVerified: %v", err)
	}
	if verified {
		t.Fatal("a superseded challenge was spent; the address nobody proved they can read is now verified")
	}
	admin, err := store.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if admin.RecoveryEmailVerifiedAt != nil {
		t.Errorf("recovery_email_verified_at = %v for %q, want nil", admin.RecoveryEmailVerifiedAt, admin.RecoveryEmail)
	}
	if admin.VerificationTokenHash != hashVerificationToken("a-different-token") {
		t.Error("the refused write still disturbed the outstanding challenge")
	}
}

// The control for the test above: the challenge the caller actually
// matched IS spent, so the guard refuses the superseded case and nothing
// else.
func TestStore_MarkRecoveryEmailVerifiedSpendsTheOutstandingChallenge(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	created := time.Now().UTC()
	provisionalAdmin(t, store, "the-mailed-token", created, created.Add(minVerificationWindow))

	verified, err := store.MarkRecoveryEmailVerified(created, hashVerificationToken("the-mailed-token"))
	if err != nil {
		t.Fatalf("MarkRecoveryEmailVerified: %v", err)
	}
	if !verified {
		t.Fatal("the outstanding challenge was refused")
	}
	admin, err := store.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if admin.RecoveryEmailVerifiedAt == nil {
		t.Error("recovery_email_verified_at is still nil after a matching redemption")
	}
	if admin.VerificationDeadline != nil || admin.VerificationTokenHash != "" || admin.VerificationTokenExpiresAt != nil {
		t.Errorf("the deadline and the challenge survived redemption: %+v", *admin)
	}

	// Single-use: the same hash a second time now matches nothing.
	again, err := store.MarkRecoveryEmailVerified(created, hashVerificationToken("the-mailed-token"))
	if err != nil {
		t.Fatalf("second MarkRecoveryEmailVerified: %v", err)
	}
	if again {
		t.Error("the same challenge was spent twice")
	}
}

// The reaper's decide/delete TOCTOU (#830 review, HIGH): the reaper reads
// a reapable record, and a verification commits at the deadline instant
// before the delete lands. An unconditional delete removes an
// administrator that is verified and permanent, seconds after its owner
// was answered 204 - and takes their sessions and their SMTP secret with
// it. The re-check under the store's own mutex is what makes the
// reaper's answer true at the instant it acts.
func TestStore_DeleteUnverifiedAdminKeepsAnAdministratorVerifiedAtTheDeadlineInstant(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	created := time.Now().UTC()
	deadline := created.Add(minVerificationWindow)
	provisionalAdmin(t, store, "the-mailed-token", created, deadline)

	// The verification commits AT the deadline - the last instant it can
	// - while a ticking reaper is reading at or after it.
	verified, err := store.MarkRecoveryEmailVerified(deadline, hashVerificationToken("the-mailed-token"))
	if err != nil || !verified {
		t.Fatalf("MarkRecoveryEmailVerified at the deadline = %v, %v; want true, nil", verified, err)
	}

	removed, deleted, err := store.DeleteUnverifiedAdmin(deadline)
	if err != nil {
		t.Fatalf("DeleteUnverifiedAdmin: %v", err)
	}
	if deleted || removed != nil {
		t.Fatal("the reaper deleted an administrator that had just been verified")
	}
	admin, err := store.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if admin == nil {
		t.Fatal("the administrator record is gone")
	}
}

// The other three states the guard has to refuse, and the one it has to
// act on: a record is reaped if and only if it is unverified, has a
// deadline, and that deadline has passed.
func TestStore_DeleteUnverifiedAdminActsOnlyOnALapsedProvisionalRecord(t *testing.T) {
	created := time.Now().UTC()
	deadline := created.Add(minVerificationWindow)

	for _, tc := range []struct {
		name       string
		arrange    func(t *testing.T, store *Store)
		now        time.Time
		wantDelete bool
	}{
		{
			name:       "before the deadline",
			arrange:    func(*testing.T, *Store) {},
			now:        deadline.Add(-time.Nanosecond),
			wantDelete: false,
		},
		{
			name: "no deadline at all (provisioned headlessly)",
			arrange: func(t *testing.T, store *Store) {
				admin, err := store.Admin()
				if err != nil {
					t.Fatalf("Admin: %v", err)
				}
				admin.VerificationDeadline = nil
				if err := store.save(storeFile{Admin: admin}); err != nil {
					t.Fatalf("save: %v", err)
				}
			},
			now:        deadline.Add(24 * time.Hour),
			wantDelete: false,
		},
		{
			name:       "lapsed and still unverified",
			arrange:    func(*testing.T, *Store) {},
			now:        deadline,
			wantDelete: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore(filepath.Join(t.TempDir(), "auth.json"))
			provisionalAdmin(t, store, "the-mailed-token", created, deadline)
			tc.arrange(t, store)

			_, deleted, err := store.DeleteUnverifiedAdmin(tc.now)
			if err != nil {
				t.Fatalf("DeleteUnverifiedAdmin: %v", err)
			}
			if deleted != tc.wantDelete {
				t.Fatalf("deleted = %v, want %v", deleted, tc.wantDelete)
			}
			admin, err := store.Admin()
			if err != nil {
				t.Fatalf("Admin: %v", err)
			}
			if (admin == nil) != tc.wantDelete {
				t.Fatalf("administrator present = %v, want %v", admin != nil, !tc.wantDelete)
			}
		})
	}
}
