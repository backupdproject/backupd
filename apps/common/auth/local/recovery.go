package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/backupdproject/backupd/apps/common/email"
)

// Account recovery: the four routes and the three messages issue #830
// adds, and the two rules that shape all of them.
//
// The first rule is that forgot-password must not be an account oracle.
// POST /forgot-password answers 204 to everything - an unenrolled
// deployment, a username that is not the administrator's, an
// administrator with no recovery address, an SMTP endpoint that refuses
// the message - because every distinguishable answer tells an
// unauthenticated caller something about the account. That extends to the
// CLOCK, which is why the send happens on a goroutine rather than inline:
// a matching username that triggers a real SMTP round trip would
// otherwise take a second or two while a non-matching one returned
// instantly, and a timing difference that large is an oracle as usable as
// a different status code. The response is written before any mail is
// attempted, always.
//
// The second is that the SMTP password is never readable. It arrives on
// the enrollment or settings request, goes straight into a 0600 file of
// its own (secrets.go), and what is persisted and returned afterwards is
// a reference and a boolean. GET /recovery has structurally no field for
// it: smtpSettingsView carries PasswordSet, not a password, so there is
// no code path - not even a buggy one - that could serialise the material
// onto a response.
//
// The remaining routes (PATCH /recovery, POST /recovery/test) are
// authenticated and the operator is waiting on them, so those DO report
// the SMTP error: the whole point of a test send is to find out what is
// wrong with the configuration you just typed.

// The three subjects this package sends. Fixed strings rather than
// operator-configurable ones: they are the only mail this product sends,
// and an operator filtering them in their client needs them stable.
const (
	confirmationSubject = "backupd: recovery email confirmed"
	resetSubject        = "backupd: password reset"
	testSubject         = "backupd: SMTP test"
)

// smtpSettingsRequest is the SMTP half of an enrollment or a settings
// update. It mirrors api/v1/openapi.json's SmtpSettings, and
// ui/shared/src/api/client.ts's own `smtp` object, field for field.
//
// Password is write-only in both directions of that mirror: the contract
// marks it writeOnly, no response schema in this package contains it, and
// an empty value on an UPDATE means "keep the one already stored" rather
// than "set an empty password" - which is what lets an operator change
// the port or the from-address on an endpoint whose API key they do not
// have in front of them, the same affordance core/service's
// storage-credential configuration already gives (#636).
type smtpSettingsRequest struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Security string `json:"security"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
}

// smtpSettingsView is what a read of the SMTP configuration answers with:
// smtpSettingsRequest minus the password, plus the one fact about it a
// surface legitimately needs.
type smtpSettingsView struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Security string `json:"security"`
	Username string `json:"username"`
	From     string `json:"from"`

	// PasswordSet is whether a password is stored at all. It is the whole
	// of what this API will say about it: enough for Settings to render
	// "leave blank to keep the stored password" honestly, and nothing a
	// caller could use to learn the value.
	PasswordSet bool `json:"passwordSet"`
}

// enrollRequest is POST /enroll's body: the credentials that used to be
// the whole of it, plus the two things #830 makes part of creating an
// administrator - the address recovery mail goes to, and the SMTP
// endpoint it goes out over.
//
// They are required rather than optional, and that is the issue's central
// requirement rather than a strictness preference: an administrator
// account with no proven way to reach its owner is an account that gets
// permanently lost the first time a password is forgotten, and there is
// no later moment at which somebody who has lost their password can
// configure SMTP.
type enrollRequest struct {
	Username      string              `json:"username"`
	Password      string              `json:"password"`
	RecoveryEmail string              `json:"recoveryEmail"`
	SMTP          smtpSettingsRequest `json:"smtp"`
}

// forgotPasswordRequest is POST /forgot-password's body. One field, and
// the response never varies with it.
type forgotPasswordRequest struct {
	Username string `json:"username"`
}

// resetPasswordRequest is POST /reset-password's body: the token out of
// the emailed link, and the password to set.
type resetPasswordRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"newPassword"`
}

// recoveryResponse is GET /recovery's and PATCH /recovery's answer: the
// recovery address, whether a confirmation has actually reached it, and
// the SMTP endpoint without its password.
type recoveryResponse struct {
	RecoveryEmail          string            `json:"recoveryEmail"`
	RecoveryEmailConfirmed bool              `json:"recoveryEmailConfirmed"`
	SMTP                   *smtpSettingsView `json:"smtp"`
}

// recoveryUpdateRequest is PATCH /recovery's body. Both members are
// pointers because both are genuinely optional: a request may change the
// recovery address, the SMTP endpoint, or both, and "absent" has to be
// distinguishable from "set to empty" for either one to be changed on its
// own.
type recoveryUpdateRequest struct {
	RecoveryEmail *string              `json:"recoveryEmail"`
	SMTP          *smtpSettingsRequest `json:"smtp"`
}

// validate checks an SMTP block the way email.Config.Validate would,
// before any password has been resolved or stored - so a malformed
// request is refused without touching the filesystem.
func (r smtpSettingsRequest) validate() error {
	return email.Config{
		Host:     r.Host,
		Port:     r.Port,
		Security: email.Security(r.Security),
		Username: r.Username,
		From:     r.From,
	}.Validate()
}

// config turns a request's SMTP block into a sendable email.Config,
// carrying the password the request itself supplied. Used on the paths
// where the material is in hand and nothing has been persisted yet:
// enrollment's confirmation send, and a settings update that supplied a
// new password.
func (r smtpSettingsRequest) config(password string) email.Config {
	return email.Config{
		Host:     r.Host,
		Port:     r.Port,
		Security: email.Security(r.Security),
		Username: r.Username,
		Password: password,
		From:     r.From,
	}
}

// view renders a persisted SMTPRecord for a read.
func viewOf(rec *SMTPRecord) *smtpSettingsView {
	if rec == nil {
		return nil
	}
	return &smtpSettingsView{
		Host:        rec.Host,
		Port:        rec.Port,
		Security:    rec.Security,
		Username:    rec.Username,
		From:        rec.From,
		PasswordSet: rec.PasswordRef != "",
	}
}

// ErrSMTPNotConfigured is returned by the send helpers when this
// deployment has no SMTP endpoint to send over. It is distinct from a send
// failure because an operator's next step differs: configure one, rather
// than fix one.
var ErrSMTPNotConfigured = errors.New("local: no SMTP configuration exists")

// smtpConfig resolves the persisted SMTP endpoint into something
// sendable, reading the password out of its 0600 file exactly once, for
// this one send.
func (s *Service) smtpConfig() (email.Config, error) {
	rec, err := s.store.SMTP()
	if err != nil {
		return email.Config{}, err
	}
	if rec == nil {
		return email.Config{}, ErrSMTPNotConfigured
	}
	cfg := email.Config{
		Host:     rec.Host,
		Port:     rec.Port,
		Security: email.Security(rec.Security),
		Username: rec.Username,
		From:     rec.From,
	}
	if rec.PasswordRef != "" {
		password, err := s.secrets.get(rec.PasswordRef)
		if err != nil {
			return email.Config{}, err
		}
		cfg.Password = password
	}
	return cfg, nil
}

// confirmationMessage is what a newly set (or newly changed) recovery
// address is sent to prove the whole chain works end to end: this
// deployment, that SMTP endpoint, that mailbox.
func confirmationMessage(to, username string) email.Message {
	return email.Message{
		To:      to,
		Subject: confirmationSubject,
		Body: "This address is now the recovery address for the Backupd administrator " +
			username + ".\n\n" +
			"If you forget that account's password, Backupd will email a single-use, " +
			"time-limited reset link here. Keep this address reachable, and keep the SMTP " +
			"connection in Backupd's settings working - together they are the only way " +
			"back into the account.\n\n" +
			"You are receiving this because the SMTP connection was tested against this " +
			"address. No action is needed.\n",
	}
}

// resetMessage is the forgotten-password link. baseURL is the deployment's
// own public address; when an operator has not told the runtime what that
// is (it has no reliable way to know - see PrintBootstrapNotice's own
// note), the bare token is sent instead of a link that would be
// confidently wrong.
func resetMessage(to, username, token, baseURL string) email.Message {
	body := "A password reset was requested for the Backupd administrator " + username + ".\n\n"
	if baseURL != "" {
		body += "Open this link to set a new password:\n\n  " + baseURL + "/reset-password?token=" + token + "\n\n"
	} else {
		body += "Open Backupd's /reset-password page and enter this token:\n\n  " + token + "\n\n"
	}
	body += "The link can be used once and expires in 30 minutes. Setting a new password " +
		"signs out every existing session.\n\n" +
		"If you did not request this, no action is needed: nothing has changed, and the " +
		"link stops working on its own.\n"
	return email.Message{To: to, Subject: resetSubject, Body: body}
}

// testMessage is what the Settings page's "send test email" button sends.
func testMessage(to string) email.Message {
	return email.Message{
		To:      to,
		Subject: testSubject,
		Body: "This is a test message from Backupd.\n\n" +
			"Receiving it means the SMTP connection in Backupd's settings works and that " +
			"account recovery can reach this address.\n",
	}
}

// send delivers one message through the configured sender, bounded by the
// request's own context.
func (s *Service) send(ctx context.Context, cfg email.Config, msg email.Message) error {
	return s.sendMail(ctx, cfg, msg)
}

// sendInBackground delivers one message off the request path, so that
// forgot-password's response time does not depend on whether the username
// matched (this file's opening note explains why that matters more than
// reporting the failure would help).
//
// The failure is logged, not lost: an operator who never receives a reset
// link needs to be able to find out why, and the runtime's own log is the
// only place that answer can go without also telling an unauthenticated
// caller whether the account exists. The message text names the host and
// the stage; email.Send is what guarantees it never names the credential.
//
// The WaitGroup is what lets this package's own tests observe the send
// deterministically (drainOutboundMail); production never waits on it,
// and a send still in flight at shutdown is one lost reset link, which is
// what a restart already means for the token itself.
func (s *Service) sendInBackground(cfg email.Config, msg email.Message) {
	s.outbound.Add(1)
	go func() {
		defer s.outbound.Done()
		ctx, cancel := context.WithTimeout(context.Background(), backgroundSendTimeout)
		defer cancel()
		if err := s.sendMail(ctx, cfg, msg); err != nil {
			s.logf("local: sending %q to the administrator's recovery address failed: %v", msg.Subject, err)
		}
	}()
}

// backgroundSendTimeout bounds a send nobody is waiting on. Longer than
// email's own per-send timeout would be pointless; shorter would cut off
// a send that was going to succeed.
const backgroundSendTimeout = 30 * time.Second

// drainOutboundMail waits for every background send started so far. It
// exists for this package's tests: forgot-password deliberately answers
// before its mail is sent, so a test that asserted on the sink
// immediately would be racing the goroutine rather than testing it.
func (s *Service) drainOutboundMail() { s.outbound.Wait() }

// handleForgotPassword implements POST /forgot-password, which answers 204
// to everything.
//
// Read the branches below as one statement: there is no path here that
// writes a different status, a different body, or a body at all. The rate
// limiter is the single exception, and it is a property of the CALLER's
// address rather than of the account - an unauthenticated route that
// sends email must have a ceiling, or it is a mail cannon pointed at the
// administrator's inbox and at the operator's SMTP quota.
func (s *Service) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	if !s.forgotLimiter.Allow(remoteIP(r, s.trustForwardedHeaders)) {
		writeAuthError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many password reset requests; wait before trying again")
		return
	}

	var req forgotPasswordRequest
	// Even a malformed body answers 204. A 400 here would distinguish
	// "you sent nonsense" from "that username is not the administrator",
	// which is harmless on its own, but the moment there are two answers
	// somebody has to keep proving the second one never depends on the
	// account, and one answer needs no such proof.
	_ = json.NewDecoder(r.Body).Decode(&req)

	// Answered BEFORE anything is looked up or sent. Every branch below
	// this line is invisible to the caller.
	w.WriteHeader(http.StatusNoContent)

	admin, err := s.store.Admin()
	if err != nil {
		s.logf("local: forgot-password could not read the administrator record: %v", err)
		return
	}
	if admin == nil || admin.Username != req.Username || admin.RecoveryEmail == "" {
		return
	}

	cfg, err := s.smtpConfig()
	if err != nil {
		s.logf("local: forgot-password has no usable SMTP configuration: %v", err)
		return
	}

	token, err := s.resetTokens.issue()
	if err != nil {
		s.logf("local: forgot-password could not issue a reset token: %v", err)
		return
	}
	s.sendInBackground(cfg, resetMessage(admin.RecoveryEmail, admin.Username, token, s.baseURL))
}

// handleResetPassword implements POST /reset-password: the other end of
// the emailed link.
//
// It consumes the token before it hashes anything, so a request that
// races another with the same token cannot have both accepted. On success
// every live session is revoked and NO new one is issued: whoever just
// set the password proves they know it by signing in with it, which also
// means a reset performed from a mail client on a phone does not hand
// that phone a live session to the console.
func (s *Service) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	if !s.resetLimiter.Allow(remoteIP(r, s.trustForwardedHeaders)) {
		writeAuthError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many password reset attempts; wait before trying again")
		return
	}

	var req resetPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAuthError(w, http.StatusBadRequest, "INVALID_REQUEST", "malformed request body")
		return
	}
	if len(req.NewPassword) < minPasswordLength {
		// Checked before the token is spent, for handleEnroll's reason: a
		// password that is too short is the operator's own typing, and
		// burning their one link over it would send them back for a
		// second email they do not need.
		writeAuthError(w, http.StatusBadRequest, "INVALID_REQUEST", "password must be at least 12 characters")
		return
	}
	if !s.resetTokens.consume(req.Token) {
		writeAuthError(w, http.StatusUnauthorized, "RESET_TOKEN_INVALID", "that password reset link has expired or has already been used")
		return
	}

	hash, err := hashPassword(req.NewPassword)
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}
	if err := s.store.SetPassword(hash); err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}

	// Every session, including any the caller happens to hold. A reset is
	// performed precisely when somebody may have lost control of the
	// account, so surviving sessions are the thing being revoked, not
	// collateral.
	s.sessions.revokeAll()
	clearSessionCookie(w, r, s.trustForwardedHeaders)
	w.WriteHeader(http.StatusNoContent)
}

// handleGetRecovery implements GET /recovery: the Settings page's read of
// the recovery address and the SMTP endpoint. Authenticated, and
// structurally incapable of returning the SMTP password (see
// smtpSettingsView).
func (s *Service) handleGetRecovery(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.authenticatedAdmin(w, r)
	if !ok {
		return
	}
	rec, err := s.store.SMTP()
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}
	writeJSON(w, http.StatusOK, recoveryResponse{
		RecoveryEmail:          admin.RecoveryEmail,
		RecoveryEmailConfirmed: admin.RecoveryEmailConfirmedAt != nil,
		SMTP:                   viewOf(rec),
	})
}

// handleUpdateRecovery implements PATCH /recovery.
//
// The order matters twice. Everything is validated before anything is
// written, so a refused request changes nothing. And when the recovery
// address changes, the confirmation message is sent BEFORE the new
// address is marked confirmed and, on failure, the whole update is
// refused - an operator must not be able to leave the account with a
// recovery address that has never received anything, which is exactly the
// silent-lockout state #830 exists to prevent.
func (s *Service) handleUpdateRecovery(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.authenticatedAdmin(w, r)
	if !ok {
		return
	}

	var req recoveryUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAuthError(w, http.StatusBadRequest, "INVALID_REQUEST", "malformed request body")
		return
	}
	if req.RecoveryEmail == nil && req.SMTP == nil {
		writeAuthError(w, http.StatusBadRequest, "INVALID_REQUEST", "name at least one of recoveryEmail or smtp to change")
		return
	}
	if req.RecoveryEmail != nil {
		if err := email.ValidateAddress(*req.RecoveryEmail); err != nil {
			writeAuthError(w, http.StatusBadRequest, "INVALID_EMAIL", "recovery email must be a valid email address")
			return
		}
	}
	if req.SMTP != nil {
		if err := req.SMTP.validate(); err != nil {
			writeAuthError(w, http.StatusBadRequest, "INVALID_REQUEST", "the SMTP configuration is incomplete: "+err.Error())
			return
		}
	}

	// The endpoint the confirmation goes out over is the one this request
	// is establishing, not the one already stored: an operator changing
	// both at once is telling us the new endpoint is the working one.
	stored, err := s.store.SMTP()
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}
	cfg, passwordRef, err := s.pendingSMTP(req.SMTP, stored)
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}

	address := admin.RecoveryEmail
	addressChanged := req.RecoveryEmail != nil && *req.RecoveryEmail != admin.RecoveryEmail
	if req.RecoveryEmail != nil {
		address = *req.RecoveryEmail
	}

	// A confirmation is sent when the address changed, and also when only
	// the SMTP endpoint changed and the current address has never been
	// confirmed: in both cases the pairing of "this endpoint" with "this
	// mailbox" is unproven, and #830's requirement is that it never stays
	// unproven silently.
	needsConfirmation := addressChanged || (req.SMTP != nil && admin.RecoveryEmailConfirmedAt == nil)
	confirmedAt := admin.RecoveryEmailConfirmedAt
	if needsConfirmation && address != "" {
		if err := s.send(r.Context(), cfg, confirmationMessage(address, admin.Username)); err != nil {
			writeAuthError(w, http.StatusBadGateway, "SMTP_SEND_FAILED", "could not send the confirmation email: "+err.Error())
			return
		}
		now := s.now().UTC()
		confirmedAt = &now
	}

	// Persisted only now that the send (if any) has succeeded. The SMTP
	// record goes first so that a recovery address recorded as confirmed
	// is never confirmed against an endpoint that was not stored.
	if req.SMTP != nil {
		if err := s.store.SetSMTP(SMTPRecord{
			Host:        req.SMTP.Host,
			Port:        req.SMTP.Port,
			Security:    req.SMTP.Security,
			Username:    req.SMTP.Username,
			PasswordRef: passwordRef,
			From:        req.SMTP.From,
		}); err != nil {
			writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
			return
		}
		// The superseded secret is removed only after the store no longer
		// points at it, so a failure above can never leave a reference to
		// a file that is gone.
		if stored != nil && stored.PasswordRef != "" && stored.PasswordRef != passwordRef {
			s.secrets.remove(stored.PasswordRef)
		}
	}
	if req.RecoveryEmail != nil || confirmedAt != admin.RecoveryEmailConfirmedAt {
		if err := s.store.SetRecoveryEmail(address, confirmedAt); err != nil {
			writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
			return
		}
	}

	updated, err := s.store.Admin()
	if err != nil || updated == nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}
	rec, err := s.store.SMTP()
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return
	}
	writeJSON(w, http.StatusOK, recoveryResponse{
		RecoveryEmail:          updated.RecoveryEmail,
		RecoveryEmailConfirmed: updated.RecoveryEmailConfirmedAt != nil,
		SMTP:                   viewOf(rec),
	})
}

// pendingSMTP works out which endpoint and which password reference an
// update should use.
//
// The interesting case is the one in the middle: an update that names an
// SMTP block WITHOUT a password keeps the stored reference and resolves
// the stored material, which is what makes "change the port without
// retyping the API key" possible. A block WITH a password stores it
// immediately (before anything else is written, so that a later refusal
// leaves an unreferenced file rather than a store pointing at nothing)
// and uses the new reference.
func (s *Service) pendingSMTP(req *smtpSettingsRequest, stored *SMTPRecord) (email.Config, string, error) {
	if req == nil {
		cfg, err := s.smtpConfig()
		if err != nil {
			return email.Config{}, "", err
		}
		ref := ""
		if stored != nil {
			ref = stored.PasswordRef
		}
		return cfg, ref, nil
	}
	if req.Password != "" {
		ref, err := s.secrets.put(req.Password)
		if err != nil {
			return email.Config{}, "", err
		}
		return req.config(req.Password), ref, nil
	}
	ref := ""
	password := ""
	if stored != nil && stored.PasswordRef != "" {
		ref = stored.PasswordRef
		resolved, err := s.secrets.get(ref)
		if err != nil {
			return email.Config{}, "", err
		}
		password = resolved
	}
	return req.config(password), ref, nil
}

// handleTestRecoveryEmail implements POST /recovery/test: send a message
// to the recovery address, now, over the stored endpoint, and report
// exactly what happened.
//
// It sends to the STORED address over the STORED endpoint, and takes no
// body at all. That is what makes it a test of the configuration rather
// than of whatever was typed into the form: an operator pressing it wants
// to know whether recovery would work today, and a button that tested a
// draft would answer a question nobody asked.
func (s *Service) handleTestRecoveryEmail(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.authenticatedAdmin(w, r)
	if !ok {
		return
	}
	if admin.RecoveryEmail == "" {
		writeAuthError(w, http.StatusBadRequest, "INVALID_EMAIL", "no recovery email is configured to send a test message to")
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
	if err := s.send(r.Context(), cfg, testMessage(admin.RecoveryEmail)); err != nil {
		writeAuthError(w, http.StatusBadGateway, "SMTP_SEND_FAILED", "could not send the test email: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// authenticatedAdmin is the session check the three settings routes share:
// a live session AND the administrator record it names. Both halves are
// required for the same reason handleRotatePassword checks both - a live
// session implies an administrator, and a route that assumed it would be
// asserting an invariant rather than checking it.
func (s *Service) authenticatedAdmin(w http.ResponseWriter, r *http.Request) (*AdminRecord, bool) {
	username, ok := s.sessions.lookup(tokenFromRequest(r))
	if !ok {
		writeAuthError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "no active session")
		return nil, false
	}
	admin, err := s.store.Admin()
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an internal error occurred")
		return nil, false
	}
	if admin == nil || admin.Username != username {
		writeAuthError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "no active session")
		return nil, false
	}
	return admin, true
}

// writeJSON serves one JSON body at status. The encode error is dropped
// deliberately and for the same reason handleSession already drops it:
// the header is already written by then, so there is nothing left to turn
// a failure into.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// logf writes one diagnostic line through the Service's configured
// logger, which is os.Stderr unless a caller replaced it.
//
// Every call site is on a path that must not report its failure to the
// caller (the forgot-password branches). Nothing passed to it ever
// carries credential material: the SMTP password is not in any error this
// package or apps/common/email produces, and a reset token is never
// formatted into a log line - only the subject of the message that
// carried it.
func (s *Service) logf(format string, args ...any) {
	if s.log == nil {
		return
	}
	fmt.Fprintf(s.log, format+"\n", args...)
}
