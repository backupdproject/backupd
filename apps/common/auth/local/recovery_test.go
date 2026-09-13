package local

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Account recovery, end to end over the real routes (#830).
//
// Everything here goes through an httptest server and a cookie jar for the
// reason handler_test.go's own opener gives, and through the mailRecorder
// seam rather than a mail server, because what these tests are about is
// the HANDLERS' behaviour around a send: that enrollment refuses when the
// send fails and leaves nothing behind, that forgot-password's answer
// never varies, that a reset is single-use and takes every session with
// it, and that the SMTP password never appears on any surface. The real
// net/smtp path has its own proofs: apps/common/email's tests drive an
// in-process server through all three security modes, and
// recovery_container_test.go drives THIS package's enrollment against an
// ephemeral mail-sink container.

// enrolledServer is the starting point for the tests that need an
// administrator with recovery configured: it enrolls the default admin
// and hands back everything needed to keep driving the API as that
// signed-in operator.
func enrolledServer(t *testing.T) (*Service, *httptest.Server, *http.Client, *mailRecorder, string) {
	t.Helper()
	svc, server, client, mail := testServerWithMail(t)
	seedCSRFCookie(t, client, server)
	csrf := csrfTokenFromJar(t, client, server)
	enrollDefaultAdmin(t, svc, server, client, csrf)
	return svc, server, client, mail, csrf
}

func TestEnroll_SendsTheVerificationMessageToTheRecoveryAddressOverTheSuppliedSMTP(t *testing.T) {
	_, _, _, mail, _ := enrolledServer(t)

	sent := mail.delivered()
	if len(sent) != 1 {
		t.Fatalf("enrollment sent %d messages, want exactly 1 (the verification)", len(sent))
	}
	got := sent[0]
	if got.msg.To != testRecoveryEmail {
		t.Errorf("the message went to %q, want the recovery address %q", got.msg.To, testRecoveryEmail)
	}
	if got.msg.Subject != verifySubject {
		t.Errorf("subject = %q, want %q", got.msg.Subject, verifySubject)
	}
	if got.cfg.Host != testSMTP().Host || got.cfg.Port != testSMTP().Port {
		t.Errorf("the message went over %s:%d, want the endpoint the request carried", got.cfg.Host, got.cfg.Port)
	}
	if got.cfg.Password != testSMTPPassword {
		t.Errorf("the send was made with password %q, want the one the request carried", got.cfg.Password)
	}
	if !strings.Contains(got.msg.Body, "bm-admin") {
		t.Errorf("the body does not name the administrator it belongs to:\n%s", got.msg.Body)
	}
}

// The acceptance criterion with the sharpest failure mode: a failing SMTP
// configuration must not produce an account. An administrator whose
// recovery address was never reachable is precisely the silent lockout
// this feature exists to prevent, so this asserts all three halves - the
// refusal, the absence of a record, and that the enrollment link still
// works afterwards, which is what makes the refusal recoverable rather
// than terminal.
func TestEnroll_IsRefusedWhenTheConfirmationCannotBeSent(t *testing.T) {
	svc, server, client, mail := testServerWithMail(t)
	seedCSRFCookie(t, client, server)
	csrf := csrfTokenFromJar(t, client, server)
	token := currentBootstrapToken(t, svc)
	mail.refuseWith(errors.New("dial tcp 10.0.0.1:587: connect: connection refused"))

	resp := postJSON(t, client, server.URL+"/api/v1/auth/enroll",
		enrollBody("bm-admin", "correct-horse-battery"),
		map[string]string{CSRFHeaderName: csrf, BootstrapTokenHeader: token})
	body := readAuthError(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("enroll status = %d, want %d (SMTP_SEND_FAILED)", resp.StatusCode, http.StatusBadGateway)
	}
	if body.Code != "SMTP_SEND_FAILED" {
		t.Errorf("error code = %q, want SMTP_SEND_FAILED", body.Code)
	}
	if !strings.Contains(body.Message, "connection refused") {
		t.Errorf("message = %q, want the SMTP error surfaced so the operator can act on it", body.Message)
	}
	if strings.Contains(body.Message, testSMTPPassword) {
		t.Fatalf("the SMTP password leaked into the refusal: %q", body.Message)
	}

	admin, err := svc.store.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if admin != nil {
		t.Fatalf("a failed verification send still created administrator %q", admin.Username)
	}

	// The same link, a second time, against a working mail server: the
	// token was verified but never spent, so the operator fixes their
	// SMTP settings and retries rather than restarting the process.
	mail.refuseWith(nil)
	retry := postJSON(t, client, server.URL+"/api/v1/auth/enroll",
		enrollBody("bm-admin", "correct-horse-battery"),
		map[string]string{CSRFHeaderName: csrf, BootstrapTokenHeader: token})
	retry.Body.Close()
	if retry.StatusCode != http.StatusNoContent {
		t.Fatalf("retry with the same enrollment link: status = %d, want %d", retry.StatusCode, http.StatusNoContent)
	}
}

func TestEnroll_RefusesAnInvalidRecoveryEmailWithoutSpendingTheLinkOrSendingAnything(t *testing.T) {
	svc, server, client, mail := testServerWithMail(t)
	seedCSRFCookie(t, client, server)
	csrf := csrfTokenFromJar(t, client, server)
	token := currentBootstrapToken(t, svc)

	bad := enrollBody("bm-admin", "correct-horse-battery")
	bad.RecoveryEmail = "not-an-address"
	resp := postJSON(t, client, server.URL+"/api/v1/auth/enroll", bad,
		map[string]string{CSRFHeaderName: csrf, BootstrapTokenHeader: token})
	body := readAuthError(t, resp)
	if resp.StatusCode != http.StatusBadRequest || body.Code != "INVALID_EMAIL" {
		t.Fatalf("status=%d code=%q, want 400 INVALID_EMAIL", resp.StatusCode, body.Code)
	}
	if mail.attempts() != 0 {
		t.Errorf("a refused recovery address still attempted %d sends", mail.attempts())
	}

	good := postJSON(t, client, server.URL+"/api/v1/auth/enroll",
		enrollBody("bm-admin", "correct-horse-battery"),
		map[string]string{CSRFHeaderName: csrf, BootstrapTokenHeader: token})
	good.Body.Close()
	if good.StatusCode != http.StatusNoContent {
		t.Fatalf("enroll after correcting the address: status = %d, want %d", good.StatusCode, http.StatusNoContent)
	}
}

// The one test in this file that does NOT replace the sender: a Service
// built without Config.SendMail must fall back to apps/common/email.Send,
// which is the wiring every production caller relies on and which no
// other test here would notice being broken. It is pointed at a real
// address nothing is listening on, so what fails is a real dial, and
// enrollment has to refuse with SMTP_SEND_FAILED and create nothing.
func TestEnroll_TheDefaultSenderIsTheRealSMTPPathAndItsFailureRefusesEnrollment(t *testing.T) {
	// Bind an ephemeral port, learn its number, release it: an address
	// that is certainly local and certainly closed.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closedPort := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	svc, err := New(Config{StorePath: filepath.Join(t.TempDir(), "auth.json"), Log: io.Discard, Notice: io.Discard})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(svc.stopReaping)
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", http.StripPrefix("/api/v1/auth", svc.Handler()))
	server := httptest.NewServer(EnsureCSRFCookie(false)(mux))
	t.Cleanup(server.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	seedCSRFCookie(t, client, server)
	csrf := csrfTokenFromJar(t, client, server)

	req := enrollBody("bm-admin", "correct-horse-battery")
	req.SMTP.Host = "127.0.0.1"
	req.SMTP.Port = closedPort
	req.SMTP.Security = "none"
	req.SMTP.Username = ""
	req.SMTP.Password = ""

	resp := postJSON(t, client, server.URL+"/api/v1/auth/enroll", req,
		map[string]string{CSRFHeaderName: csrf, BootstrapTokenHeader: currentBootstrapToken(t, svc)})
	body := readAuthError(t, resp)
	if resp.StatusCode != http.StatusBadGateway || body.Code != "SMTP_SEND_FAILED" {
		t.Fatalf("status=%d code=%q, want 502 SMTP_SEND_FAILED", resp.StatusCode, body.Code)
	}
	if !strings.Contains(body.Message, "connecting to") {
		t.Errorf("message = %q, want the real net/smtp connection failure", body.Message)
	}
	if admin, err := svc.store.Admin(); err != nil || admin != nil {
		t.Fatalf("a failed real send still created administrator %v (err=%v)", admin, err)
	}
}

func TestEnroll_RefusesAnIncompleteSMTPConfiguration(t *testing.T) {
	svc, server, client, mail := testServerWithMail(t)
	seedCSRFCookie(t, client, server)
	csrf := csrfTokenFromJar(t, client, server)
	token := currentBootstrapToken(t, svc)

	for name, mutate := range map[string]func(*enrollRequest){
		"no host":          func(r *enrollRequest) { r.SMTP.Host = "" },
		"no port":          func(r *enrollRequest) { r.SMTP.Port = 0 },
		"unknown security": func(r *enrollRequest) { r.SMTP.Security = "ssl" },
		"no from":          func(r *enrollRequest) { r.SMTP.From = "" },
	} {
		t.Run(name, func(t *testing.T) {
			req := enrollBody("bm-admin", "correct-horse-battery")
			mutate(&req)
			resp := postJSON(t, client, server.URL+"/api/v1/auth/enroll", req,
				map[string]string{CSRFHeaderName: csrf, BootstrapTokenHeader: token})
			body := readAuthError(t, resp)
			if resp.StatusCode != http.StatusBadRequest || body.Code != "INVALID_REQUEST" {
				t.Fatalf("status=%d code=%q, want 400 INVALID_REQUEST", resp.StatusCode, body.Code)
			}
		})
	}
	if mail.attempts() != 0 {
		t.Errorf("an incomplete SMTP configuration still attempted %d sends", mail.attempts())
	}
	if admin, _ := svc.store.Admin(); admin != nil {
		t.Fatal("an incomplete SMTP configuration created an administrator")
	}
}

// #830 keeps enrollment single-shot, and the interesting half is that the
// NEW work (a successful send) does not open a second door: an attempt
// with a fresh identity and a fresh recovery address is still refused
// once an administrator exists, and it never reaches the mail server at
// all, because ENROLLMENT_CLOSED is decided before anything is sent.
func TestEnroll_ASecondEnrollmentIsStillRefusedAndSendsNothing(t *testing.T) {
	svc, server, client, mail, csrf := enrolledServer(t)
	before := mail.attempts()

	second := enrollBody("someone-else", "another-long-password")
	second.RecoveryEmail = "attacker@example.test"
	resp := postJSON(t, client, server.URL+"/api/v1/auth/enroll", second,
		map[string]string{CSRFHeaderName: csrf, BootstrapTokenHeader: currentBootstrapTokenAfterEnrollment(t, svc)})
	body := readAuthError(t, resp)
	if resp.StatusCode != http.StatusForbidden || body.Code != "ENROLLMENT_CLOSED" {
		t.Fatalf("status=%d code=%q, want 403 ENROLLMENT_CLOSED", resp.StatusCode, body.Code)
	}
	if mail.attempts() != before {
		t.Errorf("a refused second enrollment sent mail (%d attempts, was %d)", mail.attempts(), before)
	}
	admin, err := svc.store.Admin()
	if err != nil || admin == nil {
		t.Fatalf("Admin: %v %v", admin, err)
	}
	if admin.Username != "bm-admin" || admin.RecoveryEmail != testRecoveryEmail {
		t.Fatalf("the administrator record was changed by the refused attempt: %+v", admin)
	}
}

// currentBootstrapTokenAfterEnrollment returns "" once enrollment has
// happened: PrintBootstrapNotice prints nothing then, which is correct,
// and the second-enrollment test needs SOME header value to send. An
// empty one is the honest thing to send - there is no live token to
// present - and ENROLLMENT_CLOSED must be the answer regardless.
func currentBootstrapTokenAfterEnrollment(t *testing.T, svc *Service) string {
	t.Helper()
	if needs, err := svc.NeedsEnrollment(); err != nil || needs {
		t.Fatalf("NeedsEnrollment = %v, %v; want false after enrollment", needs, err)
	}
	return ""
}

func TestForgotPassword_AnswersIdenticallyWhetherOrNotAnythingIsSent(t *testing.T) {
	svc, server, client, mail, csrf := enrolledServer(t)
	mail.mu.Lock()
	mail.sent = nil
	mail.calls = 0
	mail.mu.Unlock()

	for _, username := range []string{"bm-admin", "not-the-admin", ""} {
		resp := postJSON(t, client, server.URL+"/api/v1/auth/forgot-password",
			forgotPasswordRequest{Username: username}, map[string]string{CSRFHeaderName: csrf})
		payload, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("forgot-password(%q): status = %d, want 204", username, resp.StatusCode)
		}
		if len(payload) != 0 {
			t.Errorf("forgot-password(%q) answered with a body %q; every answer must be identical", username, payload)
		}
	}
	svc.drainOutboundMail()

	sent := mail.delivered()
	if len(sent) != 1 {
		t.Fatalf("%d reset messages were sent for three requests, want exactly 1 (only the matching username)", len(sent))
	}
	if sent[0].msg.To != testRecoveryEmail || sent[0].msg.Subject != resetSubject {
		t.Errorf("reset message went to %q with subject %q, want %q/%q", sent[0].msg.To, sent[0].msg.Subject, testRecoveryEmail, resetSubject)
	}
	if !strings.Contains(sent[0].msg.Body, "https://nas.example.test:8080/reset-password?token=") {
		t.Errorf("reset message carries no reset link built from the configured base URL:\n%s", sent[0].msg.Body)
	}
}

// A malformed body is the fourth way to ask the same question, and it has
// to get the same answer: the moment a caller can tell "you sent
// nonsense" from "that is not the administrator", somebody has to keep
// proving the second answer never depends on the account.
func TestForgotPassword_AnswersTheSameToAMalformedBody(t *testing.T) {
	_, server, client, _, csrf := enrolledServer(t)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/auth/forgot-password", strings.NewReader("{not json"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(CSRFHeaderName, csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 for a malformed forgot-password body", resp.StatusCode)
	}
}

// The whole recovery round trip: ask for a link, redeem the token out of
// the email, and end up with a new password, no live sessions, and a
// token that cannot be used again.
func TestResetPassword_SetsTheNewPasswordRevokesEverySessionAndCannotBeReplayed(t *testing.T) {
	svc, server, client, mail, csrf := enrolledServer(t)

	// Signed in already (enrollment issues a session), which is what
	// makes the revocation assertion below meaningful.
	if session := getJSON(t, client, server.URL+"/api/v1/auth/session"); session != http.StatusOK {
		t.Fatalf("GET /session after enroll = %d, want 200", session)
	}

	resp := postJSON(t, client, server.URL+"/api/v1/auth/forgot-password",
		forgotPasswordRequest{Username: "bm-admin"}, map[string]string{CSRFHeaderName: csrf})
	resp.Body.Close()
	svc.drainOutboundMail()
	token := resetTokenFromMail(t, mail)

	reset := postJSON(t, client, server.URL+"/api/v1/auth/reset-password",
		resetPasswordRequest{Token: token, NewPassword: "a-brand-new-passphrase"},
		map[string]string{CSRFHeaderName: csrf})
	reset.Body.Close()
	if reset.StatusCode != http.StatusNoContent {
		t.Fatalf("reset status = %d, want 204", reset.StatusCode)
	}

	// Every session is gone, including the one this client was holding.
	if session := getJSON(t, client, server.URL+"/api/v1/auth/session"); session != http.StatusUnauthorized {
		t.Errorf("GET /session after a reset = %d, want 401: a reset must revoke every live session", session)
	}

	// The old password no longer works and the new one does.
	old := postJSON(t, client, server.URL+"/api/v1/auth/login",
		credentialsRequest{Username: "bm-admin", Password: "correct-horse-battery"},
		map[string]string{CSRFHeaderName: csrf})
	old.Body.Close()
	if old.StatusCode != http.StatusUnauthorized {
		t.Errorf("login with the pre-reset password = %d, want 401", old.StatusCode)
	}
	fresh := postJSON(t, client, server.URL+"/api/v1/auth/login",
		credentialsRequest{Username: "bm-admin", Password: "a-brand-new-passphrase"},
		map[string]string{CSRFHeaderName: csrf})
	fresh.Body.Close()
	if fresh.StatusCode != http.StatusNoContent {
		t.Errorf("login with the password the reset set = %d, want 204", fresh.StatusCode)
	}

	// Single-use: the same link a second time is refused.
	replay := postJSON(t, client, server.URL+"/api/v1/auth/reset-password",
		resetPasswordRequest{Token: token, NewPassword: "yet-another-passphrase"},
		map[string]string{CSRFHeaderName: csrf})
	body := readAuthError(t, replay)
	if replay.StatusCode != http.StatusUnauthorized || body.Code != "RESET_TOKEN_INVALID" {
		t.Fatalf("replayed reset: status=%d code=%q, want 401 RESET_TOKEN_INVALID", replay.StatusCode, body.Code)
	}
	stillWorks := postJSON(t, client, server.URL+"/api/v1/auth/login",
		credentialsRequest{Username: "bm-admin", Password: "a-brand-new-passphrase"},
		map[string]string{CSRFHeaderName: csrf})
	stillWorks.Body.Close()
	if stillWorks.StatusCode != http.StatusNoContent {
		t.Errorf("the replayed reset changed the password anyway: login = %d, want 204", stillWorks.StatusCode)
	}
}

func TestResetPassword_RefusesAnUnknownTokenAndATooShortPassword(t *testing.T) {
	_, server, client, _, csrf := enrolledServer(t)

	unknown := postJSON(t, client, server.URL+"/api/v1/auth/reset-password",
		resetPasswordRequest{Token: "not-a-token-this-process-issued", NewPassword: "a-brand-new-passphrase"},
		map[string]string{CSRFHeaderName: csrf})
	body := readAuthError(t, unknown)
	if unknown.StatusCode != http.StatusUnauthorized || body.Code != "RESET_TOKEN_INVALID" {
		t.Fatalf("unknown token: status=%d code=%q, want 401 RESET_TOKEN_INVALID", unknown.StatusCode, body.Code)
	}

	short := postJSON(t, client, server.URL+"/api/v1/auth/reset-password",
		resetPasswordRequest{Token: "irrelevant", NewPassword: "short"},
		map[string]string{CSRFHeaderName: csrf})
	shortBody := readAuthError(t, short)
	if short.StatusCode != http.StatusBadRequest || shortBody.Code != "INVALID_REQUEST" {
		t.Fatalf("too-short password: status=%d code=%q, want 400 INVALID_REQUEST", short.StatusCode, shortBody.Code)
	}
}

// The reset token expires, and the clock is injected rather than waited
// on: a test that slept for the real TTL would take half an hour, and one
// that shortened the TTL would be testing a constant nothing ships with
// (bootstrap_test.go makes the same argument for the other token).
func TestResetPassword_RefusesAnExpiredToken(t *testing.T) {
	now := time.Now()
	clock := newTestClock(now)
	mail := &mailRecorder{}
	svc, err := New(Config{
		StorePath: filepath.Join(t.TempDir(), "auth.json"),
		Now:       clock.now,
		SendMail:  mail.send,
		BaseURL:   "https://nas.example.test:8080",
		Log:       io.Discard,
		Notice:    io.Discard,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(svc.stopReaping)
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", http.StripPrefix("/api/v1/auth", svc.Handler()))
	server := httptest.NewServer(EnsureCSRFCookie(false)(mux))
	t.Cleanup(server.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	seedCSRFCookie(t, client, server)
	csrf := csrfTokenFromJar(t, client, server)
	enrollDefaultAdmin(t, svc, server, client, csrf)

	resp := postJSON(t, client, server.URL+"/api/v1/auth/forgot-password",
		forgotPasswordRequest{Username: "bm-admin"}, map[string]string{CSRFHeaderName: csrf})
	resp.Body.Close()
	svc.drainOutboundMail()
	token := resetTokenFromMail(t, mail)

	clock.set(now.Add(resetTokenTTL + time.Second))
	expired := postJSON(t, client, server.URL+"/api/v1/auth/reset-password",
		resetPasswordRequest{Token: token, NewPassword: "a-brand-new-passphrase"},
		map[string]string{CSRFHeaderName: csrf})
	body := readAuthError(t, expired)
	if expired.StatusCode != http.StatusUnauthorized || body.Code != "RESET_TOKEN_INVALID" {
		t.Fatalf("expired token: status=%d code=%q, want 401 RESET_TOKEN_INVALID", expired.StatusCode, body.Code)
	}
}

func TestGetRecovery_ReportsTheSettingsWithoutTheSMTPPassword(t *testing.T) {
	_, server, client, _, _ := enrolledServer(t)

	resp, err := client.Get(server.URL + "/api/v1/auth/recovery")
	if err != nil {
		t.Fatalf("GET recovery: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET recovery status = %d, want 200; body=%s", resp.StatusCode, raw)
	}

	var got recoveryResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, raw)
	}
	if got.RecoveryEmail != testRecoveryEmail {
		t.Errorf("recoveryEmail = %q, want %q", got.RecoveryEmail, testRecoveryEmail)
	}
	if !got.RecoveryEmailConfirmed {
		t.Error("recoveryEmailConfirmed = false after an enrollment whose verification message was delivered")
	}
	if got.SMTP == nil {
		t.Fatal("smtp = null after an enrollment that configured one")
	}
	if got.SMTP.Host != testSMTP().Host || got.SMTP.Port != testSMTP().Port || got.SMTP.Security != testSMTP().Security {
		t.Errorf("smtp = %+v, want the endpoint enrollment stored", *got.SMTP)
	}
	if !got.SMTP.PasswordSet {
		t.Error("passwordSet = false although enrollment supplied an SMTP password")
	}

	// The leak assertion, against the RAW body rather than the decoded
	// struct: a field this type does not have could still be serialised
	// by a hand-written response somewhere, and the canary is what would
	// catch it.
	for _, forbidden := range []string{testSMTPPassword, "password_ref", "passwordRef"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("GET /recovery body contains %q:\n%s", forbidden, raw)
		}
	}
}

func TestRecoveryRoutes_RequireASession(t *testing.T) {
	svc, server, client, _ := testServerWithMail(t)
	seedCSRFCookie(t, client, server)
	csrf := csrfTokenFromJar(t, client, server)
	enrollDefaultAdmin(t, svc, server, client, csrf)

	// A client with no session cookie at all, but with a CSRF cookie -
	// so a 401 here is about the session and not about CSRF.
	anon := &http.Client{}
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/auth/recovery"},
		{http.MethodPatch, "/api/v1/auth/recovery"},
		{http.MethodPost, "/api/v1/auth/recovery/test"},
	} {
		req, err := http.NewRequest(tc.method, server.URL+tc.path, strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(CSRFHeaderName, csrf)
		req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: csrf})
		resp, err := anon.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		body := readAuthError(t, resp)
		if resp.StatusCode != http.StatusUnauthorized || body.Code != "UNAUTHENTICATED" {
			t.Errorf("%s %s without a session: status=%d code=%q, want 401 UNAUTHENTICATED", tc.method, tc.path, resp.StatusCode, body.Code)
		}
	}
}

func TestPatchRecovery_ChangingTheAddressReVerifiesItAndRefusesIfTheSendFails(t *testing.T) {
	svc, server, client, mail, csrf := enrolledServer(t)
	const changed = "new-admin@example.test"

	// First, a failing send: nothing may change.
	mail.refuseWith(errors.New("550 5.1.1 no such recipient"))
	resp := patchJSON(t, client, server.URL+"/api/v1/auth/recovery",
		recoveryUpdateRequest{CurrentPassword: testAdminPassword, RecoveryEmail: &[]string{changed}[0]}, map[string]string{CSRFHeaderName: csrf})
	body := readAuthError(t, resp)
	if resp.StatusCode != http.StatusBadGateway || body.Code != "SMTP_SEND_FAILED" {
		t.Fatalf("status=%d code=%q, want 502 SMTP_SEND_FAILED", resp.StatusCode, body.Code)
	}
	admin, err := svc.store.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if admin.RecoveryEmail != testRecoveryEmail {
		t.Fatalf("a refused update changed the stored address to %q", admin.RecoveryEmail)
	}

	// Then a working one: the address changes, and a verification link goes to
	// the NEW address over the stored endpoint.
	mail.refuseWith(nil)
	before := len(mail.delivered())
	ok := patchJSON(t, client, server.URL+"/api/v1/auth/recovery",
		recoveryUpdateRequest{CurrentPassword: testAdminPassword, RecoveryEmail: &[]string{changed}[0]}, map[string]string{CSRFHeaderName: csrf})
	var updated recoveryResponse
	decodeInto(t, ok, &updated)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", ok.StatusCode)
	}
	if updated.RecoveryEmail != changed || !updated.RecoveryEmailConfirmed {
		t.Fatalf("answer = %+v, want the new address, confirmed", updated)
	}
	sent := mail.delivered()
	if len(sent) != before+1 {
		t.Fatalf("%d messages sent, want one verification message", len(sent)-before)
	}
	last := sent[len(sent)-1]
	if last.msg.To != changed || last.msg.Subject != verifySubject {
		t.Errorf("the message went to %q (%q), want %q (%q)", last.msg.To, last.msg.Subject, changed, verifySubject)
	}
	// The stored password was resolved out of its file for this send: the
	// request carried no SMTP block at all.
	if last.cfg.Password != testSMTPPassword {
		t.Errorf("the message was sent with password %q, want the stored one", last.cfg.Password)
	}
}

func TestPatchRecovery_AnSMTPBlockWithNoPasswordKeepsTheStoredOne(t *testing.T) {
	svc, server, client, mail, csrf := enrolledServer(t)

	changed := testSMTP()
	changed.Port = 2525
	changed.Password = "" // "keep what is stored"
	resp := patchJSON(t, client, server.URL+"/api/v1/auth/recovery",
		recoveryUpdateRequest{CurrentPassword: testAdminPassword, SMTP: &changed}, map[string]string{CSRFHeaderName: csrf})
	var updated recoveryResponse
	decodeInto(t, resp, &updated)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if updated.SMTP == nil || updated.SMTP.Port != 2525 || !updated.SMTP.PasswordSet {
		t.Fatalf("answer = %+v, want port 2525 with a password still set", updated.SMTP)
	}

	// Proved by using it: a test send has to go out with the password
	// that was already stored, since the request supplied none.
	before := len(mail.delivered())
	test := postJSON(t, client, server.URL+"/api/v1/auth/recovery/test", struct{}{}, map[string]string{CSRFHeaderName: csrf})
	test.Body.Close()
	if test.StatusCode != http.StatusNoContent {
		t.Fatalf("test send status = %d, want 204", test.StatusCode)
	}
	sent := mail.delivered()
	if len(sent) != before+1 {
		t.Fatalf("%d messages sent by the test send, want 1", len(sent)-before)
	}
	last := sent[len(sent)-1]
	if last.cfg.Password != testSMTPPassword {
		t.Errorf("the test send used password %q, want the stored one kept by the update", last.cfg.Password)
	}
	if last.cfg.Port != 2525 {
		t.Errorf("the test send used port %d, want the updated 2525", last.cfg.Port)
	}
	if last.msg.Subject != testSubject {
		t.Errorf("test message subject = %q, want %q", last.msg.Subject, testSubject)
	}
	if err := ensureNoPasswordOnDisk(svc, testSMTPPassword); err != nil {
		t.Error(err)
	}
}

func TestPatchRecovery_RefusesAnInvalidAddressAndAnEmptyUpdate(t *testing.T) {
	_, server, client, mail, csrf := enrolledServer(t)
	before := mail.attempts()

	bad := "nope"
	resp := patchJSON(t, client, server.URL+"/api/v1/auth/recovery",
		recoveryUpdateRequest{CurrentPassword: testAdminPassword, RecoveryEmail: &bad}, map[string]string{CSRFHeaderName: csrf})
	body := readAuthError(t, resp)
	if resp.StatusCode != http.StatusBadRequest || body.Code != "INVALID_EMAIL" {
		t.Fatalf("status=%d code=%q, want 400 INVALID_EMAIL", resp.StatusCode, body.Code)
	}

	empty := patchJSON(t, client, server.URL+"/api/v1/auth/recovery",
		recoveryUpdateRequest{CurrentPassword: testAdminPassword}, map[string]string{CSRFHeaderName: csrf})
	emptyBody := readAuthError(t, empty)
	if empty.StatusCode != http.StatusBadRequest || emptyBody.Code != "INVALID_REQUEST" {
		t.Fatalf("empty update: status=%d code=%q, want 400 INVALID_REQUEST", empty.StatusCode, emptyBody.Code)
	}
	if mail.attempts() != before {
		t.Errorf("a refused update still attempted a send")
	}
}

func TestTestRecoveryEmail_ReportsTheSMTPFailure(t *testing.T) {
	_, server, client, mail, csrf := enrolledServer(t)
	mail.refuseWith(errors.New("535 5.7.8 authentication credentials invalid"))

	resp := postJSON(t, client, server.URL+"/api/v1/auth/recovery/test", struct{}{}, map[string]string{CSRFHeaderName: csrf})
	body := readAuthError(t, resp)
	if resp.StatusCode != http.StatusBadGateway || body.Code != "SMTP_SEND_FAILED" {
		t.Fatalf("status=%d code=%q, want 502 SMTP_SEND_FAILED", resp.StatusCode, body.Code)
	}
	if !strings.Contains(body.Message, "authentication credentials invalid") {
		t.Errorf("message = %q, want the SMTP error the operator has to act on", body.Message)
	}
	if strings.Contains(body.Message, testSMTPPassword) {
		t.Fatalf("the SMTP password leaked into the refusal: %q", body.Message)
	}
}

// The custody claim, checked against the filesystem rather than against
// intent: the store file holds a reference and never the material, and
// the material's own file is 0600 in a 0700 directory.
func TestSecretCustody_TheSMTPPasswordIsNeverInTheStoreFile(t *testing.T) {
	svc, _, _, _, _ := enrolledServer(t)

	if err := ensureNoPasswordOnDisk(svc, testSMTPPassword); err != nil {
		t.Fatal(err)
	}

	rec, err := svc.store.SMTP()
	if err != nil || rec == nil {
		t.Fatalf("SMTP: %v %v", rec, err)
	}
	if rec.PasswordRef == "" {
		t.Fatal("no password reference was stored, so the password was not persisted at all")
	}

	secretPath := filepath.Join(filepath.Dir(svc.store.path), smtpSecretsDirName, rec.PasswordRef)
	info, err := os.Stat(secretPath)
	if err != nil {
		t.Fatalf("the reference names no file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the SMTP password file's mode is %04o, want 0600", mode)
	}
	dir, err := os.Stat(filepath.Dir(secretPath))
	if err != nil {
		t.Fatalf("stat secrets directory: %v", err)
	}
	if mode := dir.Mode().Perm(); mode != 0o700 {
		t.Errorf("the SMTP secrets directory's mode is %04o, want 0700", mode)
	}

	// And it really is the password: a test that only proved the store
	// file lacks the string would also pass if nothing had been stored.
	stored, err := svc.secrets.get(rec.PasswordRef)
	if err != nil {
		t.Fatalf("resolve the reference: %v", err)
	}
	if stored != testSMTPPassword {
		t.Fatalf("the reference resolves to %q, want the password enrollment supplied", stored)
	}
}

// ensureNoPasswordOnDisk fails if the canary appears in the store file,
// which is the file every other part of this package reads.
func ensureNoPasswordOnDisk(svc *Service, canary string) error {
	raw, err := os.ReadFile(svc.store.path)
	if err != nil {
		return err
	}
	if strings.Contains(string(raw), canary) {
		return errors.New("the SMTP password appears in the store file:\n" + string(raw))
	}
	return nil
}

// resetTokenFromMail reads the reset token out of the message that was
// actually sent, the same way the operator's mail client would - there is
// deliberately no test-only getter for it, so what these tests redeem is
// the exact string an email carried.
func resetTokenFromMail(t *testing.T, mail *mailRecorder) string {
	t.Helper()
	sent := mail.delivered()
	for i := len(sent) - 1; i >= 0; i-- {
		if sent[i].msg.Subject != resetSubject {
			continue
		}
		_, after, ok := strings.Cut(sent[i].msg.Body, "reset-password?token=")
		if !ok {
			t.Fatalf("the reset message carries no reset link:\n%s", sent[i].msg.Body)
		}
		token := strings.Fields(after)
		if len(token) == 0 {
			t.Fatalf("could not read a token out of the reset link:\n%s", sent[i].msg.Body)
		}
		return strings.TrimSpace(token[0])
	}
	t.Fatal("no reset message was sent at all")
	return ""
}

func readAuthError(t *testing.T, resp *http.Response) authErrorResponse {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var body authErrorResponse
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode auth error (%q): %v", raw, err)
		}
	}
	return body
}

func decodeInto(t *testing.T, resp *http.Response, into any) {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode %T (%q): %v", into, raw, err)
	}
}

func getJSON(t *testing.T, client *http.Client, url string) int {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func patchJSON(t *testing.T, client *http.Client, url string, body any, headers map[string]string) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPatch, url, strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	return resp
}

// PATCH /recovery as a password-gated route (#830 security review, the
// first finding), and the SMTP endpoint as something that has to be
// PROVEN rather than merely stored (the fifth).

// The escalation the re-authentication closes, spelled out as the attack
// it is: a live session that does not know the password repoints the
// recovery address at a mailbox the caller owns, asks for a reset link,
// and takes the account over permanently. Every half of this test is a
// step on that path, and the first one has to fail.
func TestPatchRecovery_RefusesWithoutTheCurrentPasswordAndChangesNothing(t *testing.T) {
	svc, server, client, mail, csrf := enrolledServer(t)
	attacker := "attacker@example.invalid"
	hostile := testSMTP()
	hostile.Host = "smtp.attacker.invalid"

	for _, tc := range []struct {
		name string
		body recoveryUpdateRequest
	}{
		{"no password at all", recoveryUpdateRequest{RecoveryEmail: &attacker}},
		{"the wrong password", recoveryUpdateRequest{CurrentPassword: "not-the-password", RecoveryEmail: &attacker}},
		{"an SMTP repoint", recoveryUpdateRequest{CurrentPassword: "not-the-password", SMTP: &hostile}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := mail.attempts()
			resp := patchJSON(t, client, server.URL+"/api/v1/auth/recovery", tc.body,
				map[string]string{CSRFHeaderName: csrf})
			body := readAuthError(t, resp)
			if resp.StatusCode != http.StatusUnauthorized || body.Code != "UNAUTHENTICATED" {
				t.Fatalf("status=%d code=%q, want 401 UNAUTHENTICATED - the same refusal POST /auth/password gives", resp.StatusCode, body.Code)
			}
			admin, err := svc.store.Admin()
			if err != nil {
				t.Fatalf("Admin: %v", err)
			}
			if admin.RecoveryEmail != testRecoveryEmail {
				t.Fatalf("the recovery address is now %q; a session alone was enough to repoint it", admin.RecoveryEmail)
			}
			smtp, err := svc.store.SMTP()
			if err != nil {
				t.Fatalf("SMTP: %v", err)
			}
			if smtp.Host != testSMTP().Host {
				t.Fatalf("the SMTP endpoint is now %q; a session alone was enough to repoint it", smtp.Host)
			}
			// Nothing was sent either: a refused request must not have
			// made this process connect to a host the caller named.
			if mail.attempts() != before {
				t.Errorf("a refused update still attempted %d sends", mail.attempts()-before)
			}
		})
	}

	// The session is still perfectly good for everything it was good for
	// before: this is a re-authentication, not a lockout.
	ok := patchJSON(t, client, server.URL+"/api/v1/auth/recovery",
		recoveryUpdateRequest{CurrentPassword: testAdminPassword, RecoveryEmail: &attacker},
		map[string]string{CSRFHeaderName: csrf})
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("the same update with the password: status = %d, want 200", ok.StatusCode)
	}
}

// The password is checked BEFORE the SMTP password is resolved or
// stored. A refused request that had already written a secret file
// would let a caller who cannot authenticate fill the state directory,
// and - worse - would mean the check is not actually the first thing
// that happens on this route.
func TestPatchRecovery_AWrongPasswordNeverReachesTheSecretVault(t *testing.T) {
	svc, server, client, _, csrf := enrolledServer(t)
	before, err := os.ReadDir(svc.secrets.dir)
	if err != nil {
		t.Fatalf("read the secrets directory: %v", err)
	}

	withNewPassword := testSMTP()
	withNewPassword.Password = "a-brand-new-api-key"
	resp := patchJSON(t, client, server.URL+"/api/v1/auth/recovery",
		recoveryUpdateRequest{CurrentPassword: "not-the-password", SMTP: &withNewPassword},
		map[string]string{CSRFHeaderName: csrf})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	after, err := os.ReadDir(svc.secrets.dir)
	if err != nil {
		t.Fatalf("read the secrets directory: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("a refused update wrote %d new secret files", len(after)-len(before))
	}
	raw, err := os.ReadFile(svc.store.path)
	if err != nil {
		t.Fatalf("read the store: %v", err)
	}
	if strings.Contains(string(raw), "a-brand-new-api-key") {
		t.Fatal("the refused update's SMTP password reached the store file")
	}
}

// #830 review, HIGH: an SMTP-only change on a VERIFIED administrator
// used to skip the send entirely, so a new endpoint was stored while
// recoveryEmailConfirmed stayed true - a deployment reporting a working
// recovery path over an endpoint nothing has ever delivered through.
// The next forgot-password then fails silently, which is the exact
// lockout this feature exists to prevent.
func TestPatchRecovery_AChangedSMTPEndpointIsProvenBeforeItIsStored(t *testing.T) {
	svc, server, client, mail, csrf := enrolledServer(t)
	verifyTheRecoveryAddress(t, svc, server, client, mail, csrf)

	dead := testSMTP()
	dead.Host = "smtp.somewhere-else.test"
	mail.refuseWith(errors.New("dial tcp 10.0.0.9:587: connect: connection refused"))

	resp := patchJSON(t, client, server.URL+"/api/v1/auth/recovery",
		recoveryUpdateRequest{CurrentPassword: testAdminPassword, SMTP: &dead},
		map[string]string{CSRFHeaderName: csrf})
	body := readAuthError(t, resp)
	if resp.StatusCode != http.StatusBadGateway || body.Code != "SMTP_SEND_FAILED" {
		t.Fatalf("status=%d code=%q, want 502 SMTP_SEND_FAILED for an endpoint nothing could be delivered through", resp.StatusCode, body.Code)
	}
	stored, err := svc.store.SMTP()
	if err != nil {
		t.Fatalf("SMTP: %v", err)
	}
	if stored.Host != testSMTP().Host {
		t.Fatalf("the unproven endpoint %q was stored anyway", stored.Host)
	}
	view := readRecovery(t, client, server)
	if !view.RecoveryEmailConfirmed || !view.RecoveryEmailVerified {
		t.Fatalf("the refused update disturbed the proofs about the address: %+v", view)
	}

	// And the working case: the message goes out over the NEW endpoint,
	// which is what makes the answer's confirmed=true true.
	mail.refuseWith(nil)
	working := testSMTP()
	working.Host = "smtp.somewhere-else.test"
	before := len(mail.delivered())
	ok := patchJSON(t, client, server.URL+"/api/v1/auth/recovery",
		recoveryUpdateRequest{CurrentPassword: testAdminPassword, SMTP: &working},
		map[string]string{CSRFHeaderName: csrf})
	var updated recoveryResponse
	decodeInto(t, ok, &updated)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", ok.StatusCode)
	}
	sent := mail.delivered()
	if len(sent) != before+1 {
		t.Fatalf("%d messages sent for an endpoint change, want exactly 1", len(sent)-before)
	}
	last := sent[len(sent)-1]
	if last.cfg.Host != "smtp.somewhere-else.test" {
		t.Errorf("the proof went out over %q, want the endpoint this request establishes", last.cfg.Host)
	}
	if last.msg.To != testRecoveryEmail {
		t.Errorf("the proof went to %q, want the stored recovery address", last.msg.To)
	}
	// An already-verified mailbox is not un-verified by a change of
	// endpoint: what was in doubt was the endpoint, and it has just been
	// exercised.
	if !updated.RecoveryEmailVerified || !updated.RecoveryEmailConfirmed {
		t.Errorf("answer = %+v, want the address still verified and now confirmed over the new endpoint", updated)
	}
}

// The superseded secret file is REMOVED once the store no longer points
// at it, and only then. A credential belonging to nobody, left in the
// state directory by every endpoint edit, is the leak this ordering
// prevents.
func TestPatchRecovery_RemovesTheSupersededSMTPPasswordFile(t *testing.T) {
	svc, server, client, _, csrf := enrolledServer(t)
	before, err := svc.store.SMTP()
	if err != nil || before == nil || before.PasswordRef == "" {
		t.Fatalf("SMTP after enrollment = %v (%v), want a stored password reference", before, err)
	}

	rotated := testSMTP()
	rotated.Password = "the-replacement-api-key"
	resp := patchJSON(t, client, server.URL+"/api/v1/auth/recovery",
		recoveryUpdateRequest{CurrentPassword: testAdminPassword, SMTP: &rotated},
		map[string]string{CSRFHeaderName: csrf})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	after, err := svc.store.SMTP()
	if err != nil {
		t.Fatalf("SMTP: %v", err)
	}
	if after.PasswordRef == before.PasswordRef {
		t.Fatal("a replaced password reused the previous reference; a partially written file would then read as the old password")
	}
	if _, err := svc.secrets.get(before.PasswordRef); err == nil {
		t.Error("the superseded SMTP password file is still readable")
	}
	resolved, err := svc.secrets.get(after.PasswordRef)
	if err != nil {
		t.Fatalf("resolve the new reference: %v", err)
	}
	if resolved != "the-replacement-api-key" {
		t.Errorf("the new reference resolves to %q, want the password the update carried", resolved)
	}
	if err := ensureNoPasswordOnDisk(svc, "the-replacement-api-key"); err != nil {
		t.Error(err)
	}
}

// A deployment provisioned headlessly has no SMTP endpoint at all, and
// PATCH /recovery cannot prove an address over an endpoint that does not
// exist. The contract declares that refusal a 400 INVALID_REQUEST - the
// same answer POST /recovery/test and /verify-email/resend give - and it
// was a 500 until this fix, which told an operator their own
// configuration gap was a fault in the runtime.
func TestPatchRecovery_WithNoSMTPConfiguredIsABadRequestRatherThanAnInternalError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if _, err := CreateAdmin(CreateAdminConfig{
		StorePath: path,
		Username:  "bm-admin",
		Password:  testAdminPassword,
	}); err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	svc, err := New(Config{StorePath: path, SendMail: (&mailRecorder{}).send, Log: io.Discard, Notice: io.Discard})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(svc.stopReaping)
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", http.StripPrefix("/api/v1/auth", svc.Handler()))
	server := httptest.NewServer(EnsureCSRFCookie(false)(mux))
	t.Cleanup(server.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	seedCSRFCookie(t, client, server)
	csrf := csrfTokenFromJar(t, client, server)
	login := postJSON(t, client, server.URL+"/api/v1/auth/login",
		credentialsRequest{Username: "bm-admin", Password: testAdminPassword},
		map[string]string{CSRFHeaderName: csrf})
	login.Body.Close()
	if login.StatusCode != http.StatusNoContent {
		t.Fatalf("login status = %d, want 204", login.StatusCode)
	}

	address := "ops@example.test"
	resp := patchJSON(t, client, server.URL+"/api/v1/auth/recovery",
		recoveryUpdateRequest{CurrentPassword: testAdminPassword, RecoveryEmail: &address},
		map[string]string{CSRFHeaderName: csrf})
	body := readAuthError(t, resp)
	if resp.StatusCode != http.StatusBadRequest || body.Code != "INVALID_REQUEST" {
		t.Fatalf("status=%d code=%q, want 400 INVALID_REQUEST", resp.StatusCode, body.Code)
	}
}

// verifyTheRecoveryAddress redeems the link enrollment mailed, so a test
// can start from an ESTABLISHED administrator rather than a provisional
// one.
func verifyTheRecoveryAddress(t *testing.T, svc *Service, server *httptest.Server, client *http.Client, mail *mailRecorder, csrf string) {
	t.Helper()
	resp := postJSON(t, client, server.URL+"/api/v1/auth/verify-email",
		verifyEmailRequest{Token: verifyTokenFromMail(t, mail)}, map[string]string{CSRFHeaderName: csrf})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("verify-email status = %d, want 204", resp.StatusCode)
	}
	admin, err := svc.store.Admin()
	if err != nil || admin == nil || admin.RecoveryEmailVerifiedAt == nil {
		t.Fatalf("the recovery address is still unverified: %v %v", admin, err)
	}
}

// The bootstrap token is spent on the way to a successful write, and a
// write that then FAILS does not hand it back. That is the behaviour
// this pins, because it is the one an operator meets at the worst
// moment: the enrollment link is gone, no administrator exists, and the
// way forward is a restart, which mints a fresh one. A test that did not
// state it would leave the next reader guessing whether the token
// survives - and either answer is defensible until one of them is
// written down.
func TestEnroll_AStoreFailureAfterTheTokenIsSpentLeavesNoAdministratorAndNoSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	mail := &mailRecorder{}
	svc, err := New(Config{StorePath: path, SendMail: mail.send, Log: io.Discard, Notice: io.Discard})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(svc.stopReaping)
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", http.StripPrefix("/api/v1/auth", svc.Handler()))
	server := httptest.NewServer(EnsureCSRFCookie(false)(mux))
	t.Cleanup(server.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	seedCSRFCookie(t, client, server)
	csrf := csrfTokenFromJar(t, client, server)
	token := currentBootstrapToken(t, svc)

	// The secrets directory stays writable (it has its own mode) while
	// the store's own directory does not, so the SMTP password lands and
	// the token is spent before the store write is the thing that fails.
	if err := os.MkdirAll(svc.secrets.dir, 0o700); err != nil {
		t.Fatalf("create the secrets directory: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	resp := postJSON(t, client, server.URL+"/api/v1/auth/enroll",
		enrollBody("bm-admin", testAdminPassword),
		map[string]string{CSRFHeaderName: csrf, BootstrapTokenHeader: token})
	body := readAuthError(t, resp)
	if resp.StatusCode != http.StatusInternalServerError || body.Code != "INTERNAL_ERROR" {
		t.Fatalf("status=%d code=%q, want 500 INTERNAL_ERROR", resp.StatusCode, body.Code)
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod back: %v", err)
	}
	admin, err := svc.store.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if admin != nil {
		t.Fatalf("a failed store write still produced administrator %q", admin.Username)
	}
	// The secret that write would have referenced is gone rather than
	// orphaned in the state directory.
	left, err := os.ReadDir(svc.secrets.dir)
	if err != nil {
		t.Fatalf("read the secrets directory: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("%d orphaned SMTP secret files were left behind", len(left))
	}
	// And the token really was spent: the operator's way back is a
	// restart, not a retry.
	retry := postJSON(t, client, server.URL+"/api/v1/auth/enroll",
		enrollBody("bm-admin", testAdminPassword),
		map[string]string{CSRFHeaderName: csrf, BootstrapTokenHeader: token})
	retryBody := readAuthError(t, retry)
	if retry.StatusCode != http.StatusUnauthorized || retryBody.Code != "BOOTSTRAP_TOKEN_INVALID" {
		t.Fatalf("retry with the same token: status=%d code=%q, want 401 BOOTSTRAP_TOKEN_INVALID", retry.StatusCode, retryBody.Code)
	}
	svc.stopReaping()
	if err := svc.lock.release(); err != nil {
		t.Fatalf("release the store lock: %v", err)
	}
	restarted, err := New(Config{StorePath: path, SendMail: mail.send, Log: io.Discard, Notice: io.Discard})
	if err != nil {
		t.Fatalf("New after the failure: %v", err)
	}
	t.Cleanup(restarted.stopReaping)
	if fresh := currentBootstrapToken(t, restarted); fresh == "" || fresh == token {
		t.Fatal("a restart did not mint a fresh enrollment token")
	}
}
