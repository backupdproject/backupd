package local

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/backupdproject/backupd/apps/common/email"
)

// The provisional administrator: an account that exists but is not
// finished, and the reaper that finishes it one way or the other (#830
// §8-9).
//
// # Why an account can be deleted at all
//
// Everything else in this package treats the administrator record as the
// one irreversible thing it owns. This file is the exception, and the
// reason is the failure #830 exists to prevent. An account whose recovery
// address was mistyped is an account that is ALREADY lost - nobody
// notices until the password is forgotten, which is months later, and at
// that point the reset link goes to a mailbox the operator cannot read
// and the only way back in is deleting the state directory. Sending a
// message and having a mail server accept it does not rule that out: a
// typo that lands in the neighbouring domain or a colleague's inbox is
// accepted exactly as happily as the right address.
//
// So enrollment creates the account in a PROVISIONAL state, mails a
// single-use link to the address it was given, and the account becomes
// permanent when somebody opens that link. If nobody does, by a deadline
// fixed at creation, the record is deleted and enrollment reopens. The
// operator's worst case is re-running a 30-second enrollment with the
// address spelled right, instead of discovering the typo the day they
// need it.
//
// # The three properties that make that safe
//
// The deadline is computed once, at creation, and no later request can
// move it (Store.SetRecoveryEmail takes no deadline). A resend mints a
// new link inside the same window rather than extending it, so an
// attacker who can reach /verify-email/resend cannot keep an
// unverified account alive indefinitely.
//
// The reaper runs on a timer AND once at Service.New, so a process that
// was down through the whole window still cleans up on its next start -
// the deadline is a property of the record, not of any process's uptime.
//
// Deleting revokes every session in the same step. An account being
// reaped is one that may have been created by somebody who typed an
// address they do not own, and leaving their session live while the
// record disappears would be the one state in which somebody is signed
// in as an administrator that no longer exists.
//
// # What the clock is
//
// Everything here reads Service.now (Config.Now), never time.Now
// directly, including the reaper's own decision. The ticker that wakes it
// is real time; whether the deadline has passed is the injected clock's
// answer. That split is what lets a test advance an hour without
// sleeping, and it is why the reaper's decision lives in
// reapUnverifiedAdmin rather than inline in the goroutine.

// verifySubject is the subject of the one message admin creation sends.
// It IS the confirmation send #830 §4 requires - the message that proves
// the SMTP endpoint works - and it carries the verification link, because
// two separate messages to the same address at the same moment would only
// make the operator guess which one mattered.
const verifySubject = "backupd: verify your recovery email"

// verifyTokenTTL bounds how long an emailed verification link remains
// valid.
//
// The same 30 minutes the bootstrap and reset tokens get, and here that
// is not symmetry for its own sake: minVerificationWindow below is also
// 30 minutes, so a link mailed when the record is created lapses no
// earlier than the record it protects. A link that outlived the deadline
// would report success onto an account the reaper had already deleted;
// one that died earlier would strand an operator inside a window still
// open. When the window is longer (a bootstrap token issued shortly
// before enrollment pushes the deadline out), POST
// /verify-email/resend is what mints a fresh link.
const verifyTokenTTL = 30 * time.Minute

// minVerificationWindow is the created_at + 30 minutes half of #830 §9's
// deadline: the floor on how long an operator has to open the link,
// whatever the enrollment window was.
const minVerificationWindow = 30 * time.Minute

// DefaultReapInterval is how often the background reaper re-asks whether
// the provisional administrator's deadline has passed, when
// Config.ReapInterval is zero.
//
// A minute, which is coarse on purpose. The deadline is at least 30
// minutes out, nothing observes the deletion but the next request, and a
// tighter loop would buy a shorter lapse window at the cost of a store
// read per tick for the entire life of a process whose administrator is,
// in the overwhelmingly common case, verified within seconds of
// enrollment.
const DefaultReapInterval = time.Minute

// verifyEmailRequest is POST /verify-email's body: the token out of the
// emailed link, and nothing else. There is deliberately no username or
// address field - the token names the account by itself, and a request
// that also asserted whose account it was would be a second thing to
// check and a way to ask whether an address is the administrator's.
type verifyEmailRequest struct {
	Token string `json:"token"`
}

// verificationChallenge is one freshly minted verification link: the
// token to mail, the hash to persist, and when both stop counting.
type verificationChallenge struct {
	Token     string
	Hash      string
	ExpiresAt time.Time
}

// mintVerificationChallenge generates a challenge whose token is as
// strong as every other secret this package issues (24 bytes of
// crypto/rand, URL-safe so it survives being a query parameter).
func mintVerificationChallenge(now time.Time) (verificationChallenge, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return verificationChallenge{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	return verificationChallenge{
		Token:     token,
		Hash:      hashVerificationToken(token),
		ExpiresAt: now.Add(verifyTokenTTL).UTC(),
	}, nil
}

// hashVerificationToken is what the store holds instead of the token.
// SHA-256 with no salt and no stretching, deliberately: the input is 192
// bits of uniform randomness this process generated, so there is no
// dictionary to slow down and nothing a salt would separate.
func hashVerificationToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// challengeAccepts reports whether candidate is admin's outstanding,
// unexpired verification token.
//
// The comparison is constant-time over the HASHES rather than the
// tokens, which is the same reason session and bootstrap comparisons are:
// a byte-by-byte early return leaks the prefix of a live credential to
// anybody willing to time a few thousand requests.
func challengeAccepts(admin *AdminRecord, candidate string, now time.Time) bool {
	if admin == nil || candidate == "" || admin.VerificationTokenHash == "" {
		return false
	}
	if admin.VerificationTokenExpiresAt == nil || now.After(*admin.VerificationTokenExpiresAt) {
		return false
	}
	return subtle.ConstantTimeCompare(
		[]byte(admin.VerificationTokenHash),
		[]byte(hashVerificationToken(candidate)),
	) == 1
}

// verificationDeadline is #830 §9's rule, spelled once:
//
//	max(enrollment-link-active-window end, created_at + 30 minutes)
//
// Both halves answer a different question and the later one wins.
//
// created_at + 30 minutes is the floor: whoever just enrolled is sitting
// in front of the wizard, and half an hour is long enough to find the
// message in a spam folder and short enough that an abandoned typo does
// not leave a standing account.
//
// The enrollment window's end is the ceiling, and it exists because the
// bootstrap token is what authorised this account at all. While that
// token could still create an administrator, deleting one it created and
// handing back a token of the same vintage would be churn rather than
// protection: the same operator can simply re-enroll with the link they
// still have. linkWindowEnd is the zero time when this process has no
// bootstrap token to speak of (`auth create-admin`, or a Service that
// found an administrator already enrolled), in which case the floor is
// the whole rule.
func verificationDeadline(createdAt, linkWindowEnd time.Time) time.Time {
	deadline := createdAt.Add(minVerificationWindow)
	if linkWindowEnd.After(deadline) {
		return linkWindowEnd.UTC()
	}
	return deadline.UTC()
}

// verifyMessage is the message admin creation, an address change and a
// resend all send: one message, carrying the link, saying what happens
// if nobody opens it.
//
// deadline is nil when this record cannot lapse - an address changed on
// an established account (handleUpdateRecovery), whose verification
// window was consumed when it was first verified. The removal warning is
// then omitted rather than printed with an invented date: the account is
// not going anywhere, and a threat that is not true is worse than no
// threat. When there IS a deadline it is spelled out, because the body
// of this message is the only place an operator can read it and "this
// account will be removed" is not a sentence to leave implicit.
//
// baseURL is this deployment's own public address; when the runtime has
// not been told what that is (Config.BaseURL's own doc has the reason it
// cannot know), the bare token is sent with the page to paste it into,
// the same fallback resetMessage makes.
func verifyMessage(to, username, token, baseURL string, deadline *time.Time) email.Message {
	body := "This address is the recovery address for the Backupd administrator " + username +
		", and this message is the proof that Backupd can reach it.\n\n"
	if baseURL != "" {
		body += "Open this link to finish verifying it:\n\n  " + baseURL + "/verify-email?token=" + token + "\n\n"
	} else {
		body += "Open Backupd's /verify-email page and enter this token:\n\n  " + token + "\n\n"
	}
	body += "The link can be used once and expires in 30 minutes.\n\n"
	if deadline != nil {
		body += "IMPORTANT: until it is used, the administrator account is provisional. If this " +
			"address is not verified by " + deadline.UTC().Format("2006-01-02 15:04 UTC") +
			", the account will be REMOVED and Backupd will reopen enrollment, so that an " +
			"account whose recovery address nobody can read is never the one left in place.\n\n" +
			"If you did not expect this, you can ignore it: the account removes itself.\n"
	} else {
		body += "Until it is used, this address counts as unverified: account recovery would " +
			"mail a password-reset link to a mailbox nobody has proved they can read, so " +
			"Backupd keeps saying so in the console until somebody opens this link.\n"
	}
	return email.Message{To: to, Subject: verifySubject, Body: body}
}

// handleVerifyEmail implements POST /verify-email: the other end of the
// link.
//
// It is UNAUTHENTICATED, like /reset-password and for the same reason.
// The link is opened from a mail client, quite possibly on a phone that
// has never signed into this deployment, and a gate in front of it would
// require the operator to prove they hold the account in order to prove
// they hold its recovery address. The token is the credential.
//
// The reap runs FIRST, before the token is even looked at. A link
// redeemed after the deadline must not resurrect an account the reaper
// was one tick away from deleting: with the reap first, a late request
// finds no administrator and is refused, so the outcome does not depend
// on which of the two happened to run first.
func (s *Service) handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	if !s.verifyLimiter.Allow(remoteIP(r, s.trustForwardedHeaders)) {
		writeAuthError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many verification attempts; wait before trying again")
		return
	}

	var req verifyEmailRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAuthError(w, http.StatusBadRequest, "INVALID_REQUEST", "malformed request body")
		return
	}

	if _, err := s.reapUnverifiedAdmin(); err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}

	admin, err := s.store.Admin()
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}
	// One refusal for every way this can fail - unknown token, expired
	// token, already-used token, no administrator at all, an
	// administrator whose address is already verified. They are all
	// recovered the same way (sign in and ask for another link, or
	// enroll again), and distinguishing them would let an
	// unauthenticated caller probe the account's state one request at a
	// time.
	if !challengeAccepts(admin, req.Token, s.now()) {
		writeAuthError(w, http.StatusUnauthorized, "VERIFY_TOKEN_INVALID", "that verification link has expired or has already been used")
		return
	}

	if err := s.store.MarkRecoveryEmailVerified(s.now().UTC()); err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleResendVerifyEmail implements POST /verify-email/resend: mail a
// fresh link to the stored address over the stored endpoint.
//
// Authenticated, unlike the redemption above, because this one makes
// this process SEND something to an address the caller does not choose.
// The operator who can still sign in is exactly who needs it (the first
// link went to spam, or the process restarted, or the message arrived
// after they closed the tab), and requiring a session is what keeps it
// from being an unauthenticated way to mail the administrator.
//
// It reports the SMTP error verbatim, for the reason POST /recovery/test
// does: somebody is watching this one, and what is wrong with their mail
// server is the whole of what they need to know.
//
// It does NOT move the deadline (see this file's opening note). A resend
// inside a window that has nearly closed is honest about that: the new
// link carries the same deadline the old one did.
func (s *Service) handleResendVerifyEmail(w http.ResponseWriter, r *http.Request) {
	if !s.verifyLimiter.Allow(remoteIP(r, s.trustForwardedHeaders)) {
		writeAuthError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many verification emails requested; wait before trying again")
		return
	}

	admin, ok := s.authenticatedAdmin(w, r)
	if !ok {
		return
	}
	if admin.RecoveryEmail == "" {
		writeAuthError(w, http.StatusBadRequest, "INVALID_EMAIL", "no recovery email is configured to verify")
		return
	}
	if admin.RecoveryEmailVerifiedAt != nil {
		writeAuthError(w, http.StatusBadRequest, "INVALID_REQUEST", "this recovery email is already verified")
		return
	}

	cfg, err := s.smtpConfig()
	if err != nil {
		if errors.Is(err, ErrSMTPNotConfigured) {
			writeAuthError(w, http.StatusBadRequest, "INVALID_REQUEST", "no SMTP connection is configured yet")
			return
		}
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}

	now := s.now().UTC()
	challenge, err := mintVerificationChallenge(now)
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}

	// The record's own deadline, whatever it is, including nil: a resend
	// never invents or moves one.
	if err := s.send(r.Context(), cfg, verifyMessage(admin.RecoveryEmail, admin.Username, challenge.Token, s.baseURL, admin.VerificationDeadline)); err != nil {
		writeAuthError(w, http.StatusBadGateway, "SMTP_SEND_FAILED", "could not send the verification email: "+err.Error())
		return
	}

	// Persisted only once the send has succeeded, which also replaces
	// the previous challenge: an operator who asks twice has one live
	// link, the newest, rather than a growing set of them.
	if err := s.store.SetRecoveryEmail(RecoveryEmailState{
		Address:        admin.RecoveryEmail,
		ConfirmedAt:    &now,
		VerifiedAt:     nil,
		TokenHash:      challenge.Hash,
		TokenExpiresAt: &challenge.ExpiresAt,
	}); err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// reapUnverifiedAdmin deletes the administrator record, revokes every
// session and reopens enrollment when the recovery address is still
// unverified and the deadline has passed. It reports whether it deleted
// anything.
//
// Every condition is read off the record rather than remembered in this
// process, which is what makes the startup call sufficient on its own: a
// deployment that was shut down through its entire verification window
// reaches the same decision on its next start that a running timer would
// have reached at the moment the window closed.
//
// A nil deadline is never reaped. That is the state of an administrator
// provisioned with no SMTP endpoint at all (`auth create-admin`, whose
// recovery configuration is optional) and of every record written before
// #830 - neither was ever mailed a link, so neither can be blamed for
// not opening one.
//
// The fresh bootstrap token is what "reopens enrollment" means: the
// deployment is back in the state Service.New finds a never-enrolled
// store in, and PrintBootstrapNotice will print the new link. When this
// runs INSIDE New, New mints one again immediately afterwards, which is
// harmless - nothing has printed either of them yet, and the one that
// survives is the one the operator is shown.
func (s *Service) reapUnverifiedAdmin() (bool, error) {
	admin, err := s.store.Admin()
	if err != nil {
		return false, err
	}
	if admin == nil || admin.RecoveryEmailVerifiedAt != nil || admin.VerificationDeadline == nil {
		return false, nil
	}
	if s.now().UTC().Before(*admin.VerificationDeadline) {
		return false, nil
	}

	removed, err := s.store.DeleteAdmin()
	if err != nil {
		return false, err
	}
	if removed == nil {
		// Somebody else's DeleteAdmin won the race. Nothing to revoke or
		// reopen that they have not already done.
		return false, nil
	}
	if removed.PasswordRef != "" {
		// Removed only after the store no longer points at it, the same
		// order handleUpdateRecovery uses for a superseded secret.
		s.secrets.remove(removed.PasswordRef)
	}
	s.sessions.revokeAll()
	if _, err := s.bootstrap.issue(); err != nil {
		return true, err
	}
	s.logf("local: the administrator %q was removed: its recovery address %q was not verified by %s, and enrollment has reopened with a fresh bootstrap token",
		admin.Username, admin.RecoveryEmail, admin.VerificationDeadline.Format(time.RFC3339))
	return true, nil
}

// startReaper runs reapUnverifiedAdmin every reapInterval until
// stopReaping is called.
//
// The ticker is real time even when Config.Now is not, and that is the
// right split rather than an inconsistency: the ticker decides how often
// to ASK, and the injected clock decides the ANSWER. A test drives the
// answer by moving its own clock and either calling the decision
// directly or setting a short interval; production leaves both alone.
func (s *Service) startReaper() {
	go func() {
		ticker := time.NewTicker(s.reapInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.reaperStop:
				return
			case <-ticker.C:
				if _, err := s.reapUnverifiedAdmin(); err != nil {
					s.logf("local: the unverified-administrator reaper could not read or write the store: %v", err)
				}
			}
		}
	}()
}

// stopReaping ends the background reaper. Production never calls it -
// the process that owns this Service owns it for its whole life, exactly
// like the store lock this package deliberately never releases - and
// this package's own tests do, so a test binary does not accumulate one
// live goroutine per Service it builds.
func (s *Service) stopReaping() {
	s.reaperStopOnce.Do(func() { close(s.reaperStop) })
}
