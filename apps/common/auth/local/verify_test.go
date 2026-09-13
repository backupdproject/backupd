package local

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The provisional administrator, end to end (#830 §§8-9).
//
// Two things in this file are load-bearing and neither is the HTTP
// plumbing.
//
// The first is the CLOCK. Every deadline here is half an hour out, so a
// test that waited would take half an hour, and a test that shortened
// the constant would be proving something about a value nothing ships
// with (bootstrap_test.go's own note makes the same argument). So the
// Service is built with Config.Now over a variable these tests move, and
// the reaper's decision reads it - which is exactly the seam that has to
// exist for the shipped behaviour to be testable at all.
//
// The second is NON-VACUITY. A reaper test that passes because nothing
// ever created an administrator would be worthless, so each one asserts
// the record EXISTS first, moves the clock, and only then asserts it is
// gone - and the two tests either side of it (a verified account, and an
// account whose deadline has not arrived) are the controls that prove the
// deletion is caused by the condition and not by the passage of any time
// at all.

// clockedServer is testServerWithMail with the clock in the test's hands:
// the same composition (EnsureCSRFCookie over the mounted handler, a
// cookie jar, a mail recorder), plus the *time.Time every deadline in
// this file is measured against.
//
// The reap interval is left at its default, so the background timer never
// fires inside these tests: each one drives the decision directly, which
// is what makes the assertions deterministic rather than
// timing-dependent. TestReaper_TheBackgroundTimerReapsOnItsOwn is the one
// test that deliberately does the opposite.
func clockedServer(t *testing.T) (*Service, *httptest.Server, *http.Client, *mailRecorder, string, *time.Time) {
	t.Helper()
	now := time.Now().UTC()
	clock := &now
	mail := &mailRecorder{}
	svc, err := New(Config{
		StorePath: filepath.Join(t.TempDir(), "auth.json"),
		Now:       func() time.Time { return *clock },
		SendMail:  mail.send,
		BaseURL:   "https://nas.example.test:8080",
		Log:       io.Discard,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(svc.stopReaping)

	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", http.StripPrefix("/api/v1/auth", svc.Handler()))
	server := httptest.NewServer(EnsureCSRFCookie(false)(mux))
	t.Cleanup(server.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{Jar: jar}
	seedCSRFCookie(t, client, server)
	csrf := csrfTokenFromJar(t, client, server)
	return svc, server, client, mail, csrf, clock
}

// verifyTokenFromMail reads the verification token out of the message
// that was actually sent, the same way an operator's mail client would -
// there is deliberately no test-only getter, so what these tests redeem
// is the exact string an email carried.
func verifyTokenFromMail(t *testing.T, mail *mailRecorder) string {
	t.Helper()
	sent := mail.delivered()
	for i := len(sent) - 1; i >= 0; i-- {
		if sent[i].msg.Subject != verifySubject {
			continue
		}
		_, after, ok := strings.Cut(sent[i].msg.Body, "verify-email?token=")
		if !ok {
			t.Fatalf("the verification message carries no link:\n%s", sent[i].msg.Body)
		}
		fields := strings.Fields(after)
		if len(fields) == 0 {
			t.Fatalf("could not read a token out of the verification link:\n%s", sent[i].msg.Body)
		}
		return strings.TrimSpace(fields[0])
	}
	t.Fatal("no verification message was sent at all")
	return ""
}

func readRecovery(t *testing.T, client *http.Client, server *httptest.Server) recoveryResponse {
	t.Helper()
	resp, err := client.Get(server.URL + "/api/v1/auth/recovery")
	if err != nil {
		t.Fatalf("GET /recovery: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("GET /recovery status = %d, want 200", resp.StatusCode)
	}
	var out recoveryResponse
	decodeInto(t, resp, &out)
	return out
}

// #830 §8's first acceptance criterion: enrollment delivers a LINK, and
// the account it creates is provisional until somebody opens it.
func TestEnroll_CreatesAProvisionalAdministratorAndMailsAVerificationLink(t *testing.T) {
	svc, server, client, mail, csrf, clock := clockedServer(t)
	enrollDefaultAdmin(t, svc, server, client, csrf)

	sent := mail.delivered()
	if len(sent) != 1 {
		t.Fatalf("enrollment sent %d messages, want exactly 1 (the verification)", len(sent))
	}
	msg := sent[0]
	if msg.msg.To != testRecoveryEmail || msg.msg.Subject != verifySubject {
		t.Fatalf("message went to %q with subject %q, want %q / %q", msg.msg.To, msg.msg.Subject, testRecoveryEmail, verifySubject)
	}
	// A LINK, not merely a token: what the operator has to be able to do
	// is click, and the base URL the deployment was configured with is
	// what makes that possible.
	if !strings.Contains(msg.msg.Body, "https://nas.example.test:8080/verify-email?token=") {
		t.Errorf("the verification message carries no openable link:\n%s", msg.msg.Body)
	}
	// And it says what happens if nobody does, which is the only place
	// an operator can learn that the account is provisional.
	if !strings.Contains(msg.msg.Body, "REMOVED") {
		t.Errorf("the verification message does not warn that the account lapses:\n%s", msg.msg.Body)
	}

	view := readRecovery(t, client, server)
	if !view.RecoveryEmailConfirmed {
		t.Error("recoveryEmailConfirmed = false, but the mail server accepted the message")
	}
	if view.RecoveryEmailVerified {
		t.Error("recoveryEmailVerified = true before the link was ever opened")
	}
	wantDeadline := clock.Add(minVerificationWindow).UTC().Format(time.RFC3339)
	if view.VerificationDeadline != wantDeadline {
		t.Errorf("verificationDeadline = %q, want %q (created_at + 30m)", view.VerificationDeadline, wantDeadline)
	}

	admin, err := svc.store.Admin()
	if err != nil || admin == nil {
		t.Fatalf("Admin: %v %v", admin, err)
	}
	if admin.RecoveryEmailVerifiedAt != nil {
		t.Error("the persisted record is already verified")
	}
	if admin.VerificationTokenHash == "" || admin.VerificationTokenExpiresAt == nil {
		t.Fatal("the persisted record carries no verification challenge, so the mailed link could never be redeemed")
	}
}

// The link works, exactly once, and using it makes the account permanent.
func TestVerifyEmail_RedeemsTheMailedLinkOnceAndClearsTheDeadline(t *testing.T) {
	svc, server, client, mail, csrf, _ := clockedServer(t)
	enrollDefaultAdmin(t, svc, server, client, csrf)
	token := verifyTokenFromMail(t, mail)

	resp := postJSON(t, client, server.URL+"/api/v1/auth/verify-email",
		verifyEmailRequest{Token: token}, map[string]string{CSRFHeaderName: csrf})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("verify status = %d, want 204; body=%s", resp.StatusCode, body)
	}

	view := readRecovery(t, client, server)
	if !view.RecoveryEmailVerified {
		t.Error("recoveryEmailVerified = false after the link was redeemed")
	}
	if view.VerificationDeadline != "" {
		t.Errorf("verificationDeadline = %q after verification, want empty: the window was met", view.VerificationDeadline)
	}

	// Single-use. The same link, again, is refused with the code that
	// tells the UI to offer a fresh one rather than a mystery.
	replay := postJSON(t, client, server.URL+"/api/v1/auth/verify-email",
		verifyEmailRequest{Token: token}, map[string]string{CSRFHeaderName: csrf})
	replayBody := readAuthError(t, replay)
	if replay.StatusCode != http.StatusUnauthorized || replayBody.Code != "VERIFY_TOKEN_INVALID" {
		t.Fatalf("replay: status=%d code=%q, want 401 VERIFY_TOKEN_INVALID", replay.StatusCode, replayBody.Code)
	}
}

func TestVerifyEmail_RefusesAnUnknownTokenAndAnExpiredOne(t *testing.T) {
	svc, server, client, mail, csrf, clock := clockedServer(t)
	enrollDefaultAdmin(t, svc, server, client, csrf)

	unknown := postJSON(t, client, server.URL+"/api/v1/auth/verify-email",
		verifyEmailRequest{Token: "a-token-this-process-never-issued"}, map[string]string{CSRFHeaderName: csrf})
	unknownBody := readAuthError(t, unknown)
	if unknown.StatusCode != http.StatusUnauthorized || unknownBody.Code != "VERIFY_TOKEN_INVALID" {
		t.Fatalf("unknown token: status=%d code=%q, want 401 VERIFY_TOKEN_INVALID", unknown.StatusCode, unknownBody.Code)
	}

	// Expiry is tested on an ESTABLISHED account, which is the one state
	// where a token can lapse without the record lapsing with it: verify
	// first (which clears the deadline), then change the address, which
	// mails a fresh challenge to an account that can no longer be reaped.
	// Moving the clock past the token's TTL then isolates the token's own
	// expiry from the reaper entirely.
	first := postJSON(t, client, server.URL+"/api/v1/auth/verify-email",
		verifyEmailRequest{Token: verifyTokenFromMail(t, mail)}, map[string]string{CSRFHeaderName: csrf})
	first.Body.Close()
	if first.StatusCode != http.StatusNoContent {
		t.Fatalf("first verification status = %d, want 204", first.StatusCode)
	}
	changed := "moved@example.test"
	patch := patchJSON(t, client, server.URL+"/api/v1/auth/recovery",
		recoveryUpdateRequest{RecoveryEmail: &changed}, map[string]string{CSRFHeaderName: csrf})
	patch.Body.Close()
	if patch.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /recovery status = %d, want 200", patch.StatusCode)
	}
	fresh := verifyTokenFromMail(t, mail)

	*clock = clock.Add(verifyTokenTTL + time.Second)
	expired := postJSON(t, client, server.URL+"/api/v1/auth/verify-email",
		verifyEmailRequest{Token: fresh}, map[string]string{CSRFHeaderName: csrf})
	expiredBody := readAuthError(t, expired)
	if expired.StatusCode != http.StatusUnauthorized || expiredBody.Code != "VERIFY_TOKEN_INVALID" {
		t.Fatalf("expired token: status=%d code=%q, want 401 VERIFY_TOKEN_INVALID", expired.StatusCode, expiredBody.Code)
	}
	// And the account is still here: an expired VERIFICATION token on an
	// established account is not a lapsed account.
	if admin, err := svc.store.Admin(); err != nil || admin == nil {
		t.Fatalf("the administrator is gone after an expired token on an established account: %v %v", admin, err)
	}
}

// #830 §9's acceptance criterion, and the sharpest behaviour in this
// package: an unverified administrator past its deadline is DELETED, its
// sessions revoked, and enrollment reopened with a fresh bootstrap token.
func TestReaper_DeletesTheUnverifiedAdministratorAtItsDeadlineAndReopensEnrollment(t *testing.T) {
	svc, server, client, _, csrf, clock := clockedServer(t)
	enrollDefaultAdmin(t, svc, server, client, csrf)

	// The record exists and the session works, so nothing below can pass
	// vacuously.
	if code := getJSON(t, client, server.URL+"/api/v1/auth/session"); code != http.StatusOK {
		t.Fatalf("GET /session right after enrollment = %d, want 200", code)
	}
	smtp, err := svc.store.SMTP()
	if err != nil || smtp == nil || smtp.PasswordRef == "" {
		t.Fatalf("SMTP after enrollment = %v (%v), want a record with a stored password reference", smtp, err)
	}
	passwordRef := smtp.PasswordRef

	// One second BEFORE the deadline: the control. A reaper that deleted
	// on any tick at all would fail here.
	*clock = clock.Add(minVerificationWindow - time.Second)
	if deleted, err := svc.reapUnverifiedAdmin(); err != nil || deleted {
		t.Fatalf("reap before the deadline: deleted=%v err=%v, want false/nil", deleted, err)
	}
	if admin, err := svc.store.Admin(); err != nil || admin == nil {
		t.Fatalf("the administrator was removed BEFORE its deadline: %v %v", admin, err)
	}

	*clock = clock.Add(time.Second)
	deleted, err := svc.reapUnverifiedAdmin()
	if err != nil {
		t.Fatalf("reap at the deadline: %v", err)
	}
	if !deleted {
		t.Fatal("the reaper left an unverified administrator in place at its deadline")
	}

	admin, err := svc.store.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if admin != nil {
		t.Fatalf("the administrator %q still exists after its verification deadline", admin.Username)
	}
	// Sessions go with it: nobody is left signed in as an administrator
	// that no longer exists.
	if code := getJSON(t, client, server.URL+"/api/v1/auth/session"); code != http.StatusUnauthorized {
		t.Errorf("GET /session after the reap = %d, want 401", code)
	}
	// And the SMTP secret the deleted account referenced is gone from
	// disk, rather than left as a credential belonging to nobody.
	if _, err := svc.secrets.get(passwordRef); err == nil {
		t.Error("the deleted administrator's SMTP password file is still readable")
	}

	// Enrollment really is open again: the new bootstrap token this
	// process printed creates an administrator.
	reopened := currentBootstrapToken(t, svc)
	if reopened == "" {
		t.Fatal("no bootstrap token is outstanding after the reap, so enrollment did not reopen")
	}
	resp := postJSON(t, client, server.URL+"/api/v1/auth/enroll",
		enrollBody("bm-admin-2", "correct-horse-battery"),
		map[string]string{CSRFHeaderName: csrf, BootstrapTokenHeader: reopened})
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("re-enrollment after the reap = %d, want 204; body=%s", resp.StatusCode, respBody)
	}
	again, err := svc.store.Admin()
	if err != nil || again == nil || again.Username != "bm-admin-2" {
		t.Fatalf("after re-enrolling, Admin = %v (%v), want bm-admin-2", again, err)
	}
}

// The control for the test above, on the axis that matters most: a
// VERIFIED administrator is never deleted, however long the process runs.
func TestReaper_KeepsAVerifiedAdministratorAndItsSession(t *testing.T) {
	svc, server, client, mail, csrf, clock := clockedServer(t)
	enrollDefaultAdmin(t, svc, server, client, csrf)

	verify := postJSON(t, client, server.URL+"/api/v1/auth/verify-email",
		verifyEmailRequest{Token: verifyTokenFromMail(t, mail)}, map[string]string{CSRFHeaderName: csrf})
	verify.Body.Close()
	if verify.StatusCode != http.StatusNoContent {
		t.Fatalf("verify status = %d, want 204", verify.StatusCode)
	}

	*clock = clock.Add(365 * 24 * time.Hour)
	if deleted, err := svc.reapUnverifiedAdmin(); err != nil || deleted {
		t.Fatalf("reap a year after a verified enrollment: deleted=%v err=%v, want false/nil", deleted, err)
	}
	admin, err := svc.store.Admin()
	if err != nil || admin == nil {
		t.Fatalf("the verified administrator was deleted: %v %v", admin, err)
	}
	if code := getJSON(t, client, server.URL+"/api/v1/auth/session"); code != http.StatusOK {
		// The session's own 24h TTL is measured against the same clock,
		// so this asserts the reaper did not revoke it rather than that
		// sessions never expire: a year-old session is expired anyway.
		// Re-login is what makes the distinction, below.
		login := postJSON(t, client, server.URL+"/api/v1/auth/login",
			credentialsRequest{Username: "bm-admin", Password: "correct-horse-battery"},
			map[string]string{CSRFHeaderName: csrf})
		login.Body.Close()
		if login.StatusCode != http.StatusNoContent {
			t.Fatalf("signing in as the verified administrator a year later = %d, want 204", login.StatusCode)
		}
	}
}

// The timer half of #830 §9 ("a reaper enforces this on a timer AND on
// service start"). Everything else in this file drives the decision
// directly; this proves the background goroutine reaches it with no
// request arriving at all.
func TestReaper_TheBackgroundTimerReapsOnItsOwn(t *testing.T) {
	now := time.Now().UTC()
	clock := &now
	mail := &mailRecorder{}
	svc, err := New(Config{
		StorePath: filepath.Join(t.TempDir(), "auth.json"),
		Now:       func() time.Time { return *clock },
		SendMail:  mail.send,
		BaseURL:   "https://nas.example.test:8080",
		Log:       io.Discard,
		// Real milliseconds, because the TICKER is real time even when
		// the clock it consults is not (Config.ReapInterval's own doc).
		ReapInterval: 5 * time.Millisecond,
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

	// Several ticks with the deadline still ahead: the control that the
	// timer is not simply deleting whatever it finds.
	time.Sleep(50 * time.Millisecond)
	if admin, err := svc.store.Admin(); err != nil || admin == nil {
		t.Fatalf("the timer deleted an administrator before its deadline: %v %v", admin, err)
	}

	*clock = now.Add(minVerificationWindow + time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for {
		admin, err := svc.store.Admin()
		if err != nil {
			t.Fatalf("Admin: %v", err)
		}
		if admin == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the background reaper never deleted the lapsed administrator, with no request made")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The start-up half: a process that was DOWN through the whole
// verification window still cleans up, because the deadline lives on the
// record rather than in any process's memory.
func TestServiceNew_ReapsALapsedAdministratorAtStartupAndReopensEnrollment(t *testing.T) {
	now := time.Now().UTC()
	clock := &now
	mail := &mailRecorder{}
	storePath := filepath.Join(t.TempDir(), "auth.json")
	newService := func() *Service {
		svc, err := New(Config{
			StorePath: storePath,
			Now:       func() time.Time { return *clock },
			SendMail:  mail.send,
			BaseURL:   "https://nas.example.test:8080",
			Log:       io.Discard,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return svc
	}

	first := newService()
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", http.StripPrefix("/api/v1/auth", first.Handler()))
	server := httptest.NewServer(EnsureCSRFCookie(false)(mux))
	t.Cleanup(server.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	seedCSRFCookie(t, client, server)
	csrf := csrfTokenFromJar(t, client, server)
	enrollDefaultAdmin(t, first, server, client, csrf)

	// The process stops - the store lock released the way a real exit
	// releases it - and the window passes with nothing running.
	first.stopReaping()
	if err := first.lock.release(); err != nil {
		t.Fatalf("release the store lock: %v", err)
	}
	*clock = now.Add(2 * time.Hour)

	second := newService()
	t.Cleanup(second.stopReaping)
	admin, err := second.store.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if admin != nil {
		t.Fatalf("a restart left the lapsed administrator %q in place", admin.Username)
	}
	// And this start printed an enrollment token, which is what "reopens
	// enrollment" has to mean for the operator reading the log.
	if token := currentBootstrapToken(t, second); token == "" {
		t.Fatal("the restart issued no bootstrap token, so enrollment did not reopen")
	}
}

// The resend path #830 §9 requires beside the banner ("surface an
// unverified state in the UI, and re-send option"): a fresh link, and the
// previous one dead so two live links can never both be outstanding.
func TestResendVerifyEmail_MailsAFreshLinkAndKillsThePreviousOne(t *testing.T) {
	svc, server, client, mail, csrf, _ := clockedServer(t)
	enrollDefaultAdmin(t, svc, server, client, csrf)
	original := verifyTokenFromMail(t, mail)

	resend := postJSON(t, client, server.URL+"/api/v1/auth/verify-email/resend", struct{}{},
		map[string]string{CSRFHeaderName: csrf})
	resend.Body.Close()
	if resend.StatusCode != http.StatusNoContent {
		t.Fatalf("resend status = %d, want 204", resend.StatusCode)
	}
	replacement := verifyTokenFromMail(t, mail)
	if replacement == original {
		t.Fatal("the resend mailed the same token again, so an intercepted first link is still live")
	}

	stale := postJSON(t, client, server.URL+"/api/v1/auth/verify-email",
		verifyEmailRequest{Token: original}, map[string]string{CSRFHeaderName: csrf})
	staleBody := readAuthError(t, stale)
	if stale.StatusCode != http.StatusUnauthorized || staleBody.Code != "VERIFY_TOKEN_INVALID" {
		t.Fatalf("the superseded link: status=%d code=%q, want 401 VERIFY_TOKEN_INVALID", stale.StatusCode, staleBody.Code)
	}

	good := postJSON(t, client, server.URL+"/api/v1/auth/verify-email",
		verifyEmailRequest{Token: replacement}, map[string]string{CSRFHeaderName: csrf})
	good.Body.Close()
	if good.StatusCode != http.StatusNoContent {
		t.Fatalf("the resent link: status = %d, want 204", good.StatusCode)
	}
	if !readRecovery(t, client, server).RecoveryEmailVerified {
		t.Error("the resent link was accepted but the address is not recorded as verified")
	}
}

// The resend is authenticated and the redemption is not, and both halves
// of that matter: the one that SENDS mail to an address the caller does
// not choose needs a session, the one redeemed from a mail client on a
// phone cannot have one.
func TestResendVerifyEmail_RequiresASessionAndRefusesOnceVerified(t *testing.T) {
	svc, server, client, mail, csrf, _ := clockedServer(t)
	enrollDefaultAdmin(t, svc, server, client, csrf)

	anonymous := &http.Client{}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/auth/verify-email/resend", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set(CSRFHeaderName, csrf)
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: csrf})
	resp, err := anonymous.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	body := readAuthError(t, resp)
	if resp.StatusCode != http.StatusUnauthorized || body.Code != "UNAUTHENTICATED" {
		t.Fatalf("resend without a session: status=%d code=%q, want 401 UNAUTHENTICATED", resp.StatusCode, body.Code)
	}

	verify := postJSON(t, client, server.URL+"/api/v1/auth/verify-email",
		verifyEmailRequest{Token: verifyTokenFromMail(t, mail)}, map[string]string{CSRFHeaderName: csrf})
	verify.Body.Close()
	if verify.StatusCode != http.StatusNoContent {
		t.Fatalf("verify status = %d, want 204", verify.StatusCode)
	}
	already := postJSON(t, client, server.URL+"/api/v1/auth/verify-email/resend", struct{}{},
		map[string]string{CSRFHeaderName: csrf})
	alreadyBody := readAuthError(t, already)
	if already.StatusCode != http.StatusBadRequest || alreadyBody.Code != "INVALID_REQUEST" {
		t.Fatalf("resend for a verified address: status=%d code=%q, want 400 INVALID_REQUEST", already.StatusCode, alreadyBody.Code)
	}
}

// The custody claim for the new credential, checked against the two
// surfaces it could leak onto and the one file it is compared against.
func TestVerifyEmail_TheTokenIsNeverReadableFromTheStoreTheAPIOrTheLog(t *testing.T) {
	now := time.Now().UTC()
	clock := &now
	mail := &mailRecorder{}
	var log strings.Builder
	svc, err := New(Config{
		StorePath: filepath.Join(t.TempDir(), "auth.json"),
		Now:       func() time.Time { return *clock },
		SendMail:  mail.send,
		BaseURL:   "https://nas.example.test:8080",
		Log:       &log,
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
	token := verifyTokenFromMail(t, mail)

	// The store holds the HASH, so the token itself must not appear in
	// the file that everything else in this package reads.
	raw, err := os.ReadFile(svc.store.path)
	if err != nil {
		t.Fatalf("read the store: %v", err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatalf("the verification token is in the store file:\n%s", raw)
	}
	// The control for that assertion: the hash IS there, so the search
	// above is not passing because nothing was persisted at all.
	if !strings.Contains(string(raw), hashVerificationToken(token)) {
		t.Fatalf("the store holds no challenge for the token that was mailed:\n%s", raw)
	}

	// No API surface reports it, including the authenticated read the
	// Settings page makes and the banner reads.
	resp, err := client.Get(server.URL + "/api/v1/auth/recovery")
	if err != nil {
		t.Fatalf("GET /recovery: %v", err)
	}
	readBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, forbidden := range []string{token, hashVerificationToken(token), "verification_token_hash", "verificationTokenHash"} {
		if strings.Contains(string(readBody), forbidden) {
			t.Errorf("GET /recovery returned %q:\n%s", forbidden, readBody)
		}
	}

	// And the reaper's own log line names the account and the deadline,
	// never the credential.
	*clock = now.Add(2 * minVerificationWindow)
	if _, err := svc.reapUnverifiedAdmin(); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if log.Len() == 0 {
		t.Fatal("the reaper deleted an administrator and logged nothing, so an operator has no way to find out why")
	}
	if strings.Contains(log.String(), token) || strings.Contains(log.String(), hashVerificationToken(token)) {
		t.Fatalf("the verification token leaked into the log:\n%s", log.String())
	}
}

// #830 §9's deadline rule, at the one level where both halves of its max
// can actually be exercised: with the shipped TTLs the floor always wins
// (a bootstrap token is issued before the record it authorises is
// created), so this is what keeps the OTHER half honest if either
// constant ever moves.
func TestVerificationDeadline_IsTheLaterOfTheEnrollmentWindowAndTheFloor(t *testing.T) {
	created := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	if got, want := verificationDeadline(created, created.Add(5*time.Minute)), created.Add(minVerificationWindow); !got.Equal(want) {
		t.Errorf("with a window ending before the floor: deadline = %s, want %s", got, want)
	}
	if got, want := verificationDeadline(created, created.Add(4*time.Hour)), created.Add(4*time.Hour); !got.Equal(want) {
		t.Errorf("with a window ending after the floor: deadline = %s, want %s", got, want)
	}
	// No enrollment window at all (`auth create-admin`): the floor is
	// the whole rule, and a zero time must never be read as "already
	// expired" or as "in 1970".
	if got, want := verificationDeadline(created, time.Time{}), created.Add(minVerificationWindow); !got.Equal(want) {
		t.Errorf("with no enrollment window: deadline = %s, want %s", got, want)
	}
}
