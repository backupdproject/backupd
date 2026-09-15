package local

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/retnd/retnd/core/tests/dockerlease"
)

// The account-recovery mail path against a REAL SMTP server, in an
// ephemeral container this test starts and removes.
//
// Why this exists on top of recovery_test.go: every test in that file
// replaces the sender, so all of them would still pass if net/smtp were
// never reached at all. What is unproven there is the thing an operator
// actually depends on - that a message this product composes is accepted
// by a mail server, lands in a mailbox, and carries a reset link somebody
// can use. That claim can only be made against a server, and the
// project's hermeticity rule says which server: one this test brings up
// and tears down itself, never a real mail service and never ambient
// credentials.
//
// inbucket is the sink because it is a mail sink and nothing else: it
// accepts SMTP on 2500, stores what it receives in memory, and exposes it
// over a small HTTP API. Nothing leaves the container, there is no
// account to configure, and the image is multi-arch.
//
// The teardown is a `docker rm -f` in t.Cleanup, which covers a pass, a
// t.Fatalf and a panic alike, plus dockerlease.Sweep on the way IN, which
// is what covers the case t.Cleanup cannot: a killed test binary (a
// `go test` timeout, a Ctrl-C) takes its own cleanup with it, and the
// sweep is how the next run removes what the killed one left
// (core/tests/dockerlease's own doc has the history).

const (
	// mailSinkImage is the ephemeral SMTP sink. :latest matches the
	// convention core/tests/machines already uses for its own fixture
	// images (minioImage): these are throwaway fixtures whose exact
	// version is not part of what any test asserts, and pinning one here
	// would be a version to bump rather than a guarantee to keep.
	mailSinkImage = "inbucket/inbucket:latest"

	// The sink's own ports inside the container. Both are published on
	// 127.0.0.1 with an ephemeral host port, so several worktrees can run
	// this at once and nothing is reachable off this machine.
	mailSinkSMTPPort = "2500/tcp"
	mailSinkHTTPPort = "9000/tcp"
)

// requireMailSinkDocker gates every test in this file, and decides
// whether an absent docker is a skip or a failure.
//
// The split is core/tests/dockerlease's, deliberately copied rather than
// softened (#456): on a developer laptop with no docker, skipping is
// honest, because this file is evidence about the mail path rather than a
// requirement on everyone's machine. Inside the local gate, docker is a
// declared prerequisite, so the same condition means the gate's own
// machine is broken - and a skip there would quietly delete the only
// proof that this product can send mail at all while the run went on
// printing ok. Under CI_LOCAL=1 this is therefore a failure carrying the
// same greppable INFRA: marker every other fixture in this repository
// uses, and CI_LOCAL_SKIP_DOCKER=1 remains the gate's own documented
// opt-out (which already ledgers the run INCOMPLETE).
func requireMailSinkDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		mailSinkUnavailable(t, "no docker binary on PATH: %v", err)
		return
	}
	if err := exec.Command("docker", "version").Run(); err != nil {
		mailSinkUnavailable(t, "`docker version` did not answer: %v", err)
	}
}

func mailSinkUnavailable(t *testing.T, reason string, args ...any) {
	t.Helper()
	detail := fmt.Sprintf(reason, args...)
	if os.Getenv("CI_LOCAL") == "1" && os.Getenv("CI_LOCAL_SKIP_DOCKER") != "1" {
		t.Fatalf("INFRA: local-auth mail sink: %s\nDocker is a declared prerequisite of this gate (CI_LOCAL=1), so this is an INFRASTRUCTURE failure and not a product one. Skipping here would take the only proof that account recovery can actually send mail out of the run while the gate still printed ok.", detail)
	}
	t.Skipf("local-auth mail sink: SKIPPING (missing capability: %s)", detail)
}

// mailSink is one running sink: the host address to send to, and the API
// to read what arrived.
type mailSink struct {
	smtpHost string
	smtpPort int
	apiBase  string
}

// startMailSink runs the sink, waits until BOTH of its ports actually
// answer, and removes the container when the test finishes.
//
// Readiness is observed rather than assumed. `docker run -d` returns when
// the container has been created, not when the process inside it is
// listening, and Docker's published-port proxy accepts a TCP connection
// from the moment the port is published - so a send issued too early
// fails with a connection reset that looks exactly like the product bug
// this file exists to rule out.
func startMailSink(t *testing.T) mailSink {
	t.Helper()
	requireMailSinkDocker(t)
	dockerlease.Sweep()

	name := "backupd-auth-mailsink-" + strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()) + "-" + time.Now().Format("150405.000000")
	args := []string{
		"run", "-d", "--name", name,
		dockerlease.LabelFlag, dockerlease.LabelSpec,
		"-p", "127.0.0.1::2500",
		"-p", "127.0.0.1::9000",
		mailSinkImage,
	}
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		mailSinkUnavailable(t, "`docker run %s` failed: %v\n%s", mailSinkImage, err, out)
		return mailSink{}
	}
	// Registered immediately after a successful run, so a failure in the
	// readiness wait below still removes the container.
	t.Cleanup(func() {
		if t.Failed() {
			if logs, err := exec.Command("docker", "logs", name).CombinedOutput(); err == nil {
				t.Logf("mail sink logs:\n%s", logs)
			}
		}
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})

	smtpHost, smtpPort := publishedPort(t, name, mailSinkSMTPPort)
	_, apiPort := publishedPort(t, name, mailSinkHTTPPort)
	sink := mailSink{
		smtpHost: smtpHost,
		smtpPort: smtpPort,
		apiBase:  fmt.Sprintf("http://127.0.0.1:%d", apiPort),
	}

	deadline := time.Now().Add(60 * time.Second)
	for {
		if smtpAnswers(sink) && apiAnswers(sink) {
			return sink
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
			t.Fatalf("INFRA: the mail sink never started listening on %s and %s within 60s; logs:\n%s", mailSinkSMTPPort, mailSinkHTTPPort, logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func publishedPort(t *testing.T, container, port string) (string, int) {
	t.Helper()
	out, err := exec.Command("docker", "port", container, port).Output()
	if err != nil {
		t.Fatalf("INFRA: docker port %s %s: %v", container, port, err)
	}
	first := strings.TrimSpace(strings.Split(strings.TrimSpace(string(out)), "\n")[0])
	host, p, err := net.SplitHostPort(first)
	if err != nil {
		t.Fatalf("INFRA: cannot read %q as a published address: %v", first, err)
	}
	n := 0
	if _, err := fmt.Sscanf(p, "%d", &n); err != nil || n == 0 {
		t.Fatalf("INFRA: cannot read %q as a port", p)
	}
	return host, n
}

func smtpAnswers(sink mailSink) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(sink.smtpHost, fmt.Sprint(sink.smtpPort)), 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func apiAnswers(sink mailSink) bool {
	resp, err := http.Get(sink.apiBase + "/api/v1/mailbox/readiness-probe")
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// sinkMessage is the part of inbucket's message JSON these tests read.
type sinkMessage struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	Subject string `json:"subject"`
	Body    struct {
		Text string `json:"text"`
	} `json:"body"`
}

// waitForMessage polls the sink's mailbox until a message with subject
// arrives, and returns it with its body fetched.
//
// Polled rather than waited on once: SMTP delivery is asynchronous on the
// server's side, and this is the one place in these tests where "not yet"
// and "never" are genuinely different. A bounded poll distinguishes them;
// a single read would report a race as a product failure.
func waitForMessage(t *testing.T, sink mailSink, mailbox, subject string) sinkMessage {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		for _, m := range mailboxMessages(t, sink, mailbox) {
			if m.Subject != subject {
				continue
			}
			return fetchMessage(t, sink, mailbox, m.ID)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no message with subject %q arrived in mailbox %q within 30s (mailbox holds %d messages)",
				subject, mailbox, len(mailboxMessages(t, sink, mailbox)))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func mailboxMessages(t *testing.T, sink mailSink, mailbox string) []sinkMessage {
	t.Helper()
	resp, err := http.Get(sink.apiBase + "/api/v1/mailbox/" + mailbox)
	if err != nil {
		t.Fatalf("INFRA: reading the sink's mailbox: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var msgs []sinkMessage
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		t.Fatalf("INFRA: decoding the sink's mailbox listing: %v", err)
	}
	return msgs
}

func fetchMessage(t *testing.T, sink mailSink, mailbox, id string) sinkMessage {
	t.Helper()
	resp, err := http.Get(sink.apiBase + "/api/v1/mailbox/" + mailbox + "/" + id)
	if err != nil {
		t.Fatalf("INFRA: reading a message from the sink: %v", err)
	}
	defer resp.Body.Close()
	var msg sinkMessage
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatalf("INFRA: decoding a message from the sink: %v", err)
	}
	return msg
}

// sinkServer is a Service wired for REAL sending (Config.SendMail left
// nil, so apps/common/email.Send is what runs), behind the same
// EnsureCSRFCookie composition every other test in this package uses.
func sinkServer(t *testing.T) (*Service, *httptest.Server, *http.Client, string) {
	t.Helper()
	svc, err := New(Config{
		StorePath: filepath.Join(t.TempDir(), "auth.json"),
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
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{Jar: jar}
	seedCSRFCookie(t, client, server)
	return svc, server, client, csrfTokenFromJar(t, client, server)
}

// sinkEnrollBody is enrollBody pointed at the running container, in the
// one security mode a plaintext sink speaks. The encrypted modes are
// proved against an in-process TLS server in apps/common/email's own
// tests; what this file adds is a real server on the other end of a real
// submission conversation.
func sinkEnrollBody(sink mailSink, recoveryEmail string) enrollRequest {
	return enrollRequest{
		Username:      "bm-admin",
		Password:      "correct-horse-battery",
		RecoveryEmail: recoveryEmail,
		SMTP: smtpSettingsRequest{
			Host:     sink.smtpHost,
			Port:     sink.smtpPort,
			Security: "none",
			From:     "backupd@example.test",
		},
	}
}

// #830 §8's acceptance criterion against a REAL mail server: enrollment
// delivers a verification LINK to the recovery address, the account is
// unverified until that link is used, and using the exact string the
// message carried verifies it.
//
// The link is read out of the delivered message rather than out of this
// process, which is the whole point of doing it here: recovery_test.go
// replaces the sender, so it could not catch a message whose link was
// mangled on the way through a real SMTP conversation (a wrapped line, a
// token character a header encoder decided to escape).
func TestContainer_EnrollmentDeliversAUsableVerificationLinkOverRealSMTP(t *testing.T) {
	sink := startMailSink(t)
	svc, server, client, csrf := sinkServer(t)
	token := currentBootstrapToken(t, svc)
	const recoveryEmail = "confirm-me@example.test"

	resp := postJSON(t, client, server.URL+"/api/v1/auth/enroll",
		sinkEnrollBody(sink, recoveryEmail),
		map[string]string{CSRFHeaderName: csrf, BootstrapTokenHeader: token})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("enroll status = %d, want 204; body=%s", resp.StatusCode, body)
	}

	msg := waitForMessage(t, sink, "confirm-me", verifySubject)
	if !strings.Contains(msg.From, "backupd@example.test") {
		t.Errorf("the message's from = %q, want the configured from-address", msg.From)
	}
	if !strings.Contains(msg.Body.Text, "bm-admin") {
		t.Errorf("the body does not name the administrator:\n%s", msg.Body.Text)
	}
	if !strings.Contains(msg.Body.Text, "recovery address") {
		t.Errorf("the body does not explain what it is for:\n%s", msg.Body.Text)
	}

	// Unverified until the link is used, and the deadline is published so
	// the console can say by when.
	before := sinkRecovery(t, client, server)
	if before.RecoveryEmailVerified {
		t.Fatal("the account is verified before anybody opened the link")
	}
	if before.VerificationDeadline == "" {
		t.Error("no verification deadline was published for a provisional account")
	}

	_, after, ok := strings.Cut(msg.Body.Text, "verify-email?token=")
	if !ok {
		t.Fatalf("the delivered message carries no verification link:\n%s", msg.Body.Text)
	}
	verifyToken := strings.TrimSpace(strings.Fields(after)[0])
	if verifyToken == "" {
		t.Fatalf("the delivered link carries no token:\n%s", msg.Body.Text)
	}

	verify := postJSON(t, client, server.URL+"/api/v1/auth/verify-email",
		verifyEmailRequest{Token: verifyToken}, map[string]string{CSRFHeaderName: csrf})
	verifyBody, _ := io.ReadAll(verify.Body)
	verify.Body.Close()
	if verify.StatusCode != http.StatusNoContent {
		t.Fatalf("verify status = %d, want 204; body=%s", verify.StatusCode, verifyBody)
	}

	settled := sinkRecovery(t, client, server)
	if !settled.RecoveryEmailVerified || settled.VerificationDeadline != "" {
		t.Fatalf("after redeeming the delivered link: %+v, want verified with no deadline", settled)
	}
	// The account survives its original deadline now, which is the whole
	// point of verifying: the reaper has nothing to act on.
	if deleted, err := svc.reapUnverifiedAdmin(); err != nil || deleted {
		t.Fatalf("reap after verification: deleted=%v err=%v, want false/nil", deleted, err)
	}
}

// sinkRecovery reads GET /recovery as the signed-in operator, which is
// where the unverified state and its deadline are published.
func sinkRecovery(t *testing.T, client *http.Client, server *httptest.Server) recoveryResponse {
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

// The whole recovery path over a real mail server: enroll, forget the
// password, read the link out of the delivered message, and use it.
func TestContainer_ForgotPasswordDeliversAUsableResetLink(t *testing.T) {
	sink := startMailSink(t)
	svc, server, client, csrf := sinkServer(t)
	token := currentBootstrapToken(t, svc)

	enroll := postJSON(t, client, server.URL+"/api/v1/auth/enroll",
		sinkEnrollBody(sink, "reset-me@example.test"),
		map[string]string{CSRFHeaderName: csrf, BootstrapTokenHeader: token})
	enroll.Body.Close()
	if enroll.StatusCode != http.StatusNoContent {
		t.Fatalf("enroll status = %d, want 204", enroll.StatusCode)
	}
	waitForMessage(t, sink, "reset-me", verifySubject)

	forgot := postJSON(t, client, server.URL+"/api/v1/auth/forgot-password",
		forgotPasswordRequest{Username: "bm-admin"}, map[string]string{CSRFHeaderName: csrf})
	forgot.Body.Close()
	if forgot.StatusCode != http.StatusNoContent {
		t.Fatalf("forgot-password status = %d, want 204", forgot.StatusCode)
	}

	msg := waitForMessage(t, sink, "reset-me", resetSubject)
	_, after, ok := strings.Cut(msg.Body.Text, "reset-password?token=")
	if !ok {
		t.Fatalf("the delivered reset message carries no link:\n%s", msg.Body.Text)
	}
	resetToken := strings.TrimSpace(strings.Fields(after)[0])
	if resetToken == "" {
		t.Fatalf("the delivered reset link carries no token:\n%s", msg.Body.Text)
	}

	reset := postJSON(t, client, server.URL+"/api/v1/auth/reset-password",
		resetPasswordRequest{Token: resetToken, NewPassword: "a-brand-new-passphrase"},
		map[string]string{CSRFHeaderName: csrf})
	reset.Body.Close()
	if reset.StatusCode != http.StatusNoContent {
		t.Fatalf("reset status = %d, want 204", reset.StatusCode)
	}

	// The session enrollment issued is gone, and the emailed reset really
	// did change the password.
	if code := getJSON(t, client, server.URL+"/api/v1/auth/session"); code != http.StatusUnauthorized {
		t.Errorf("GET /session after the emailed reset = %d, want 401", code)
	}
	login := postJSON(t, client, server.URL+"/api/v1/auth/login",
		credentialsRequest{Username: "bm-admin", Password: "a-brand-new-passphrase"},
		map[string]string{CSRFHeaderName: csrf})
	login.Body.Close()
	if login.StatusCode != http.StatusNoContent {
		t.Fatalf("login with the password the emailed link set = %d, want 204", login.StatusCode)
	}
}

// The Settings-page action, against the real server, plus the leak
// assertion that matters most for it: a test send that works must not put
// the SMTP password anywhere an operator could read it back.
func TestContainer_TestSendReachesTheSinkAndLeaksNoCredential(t *testing.T) {
	sink := startMailSink(t)
	svc, server, client, csrf := sinkServer(t)
	token := currentBootstrapToken(t, svc)

	body := sinkEnrollBody(sink, "settings@example.test")
	// A password on a plaintext LOOPBACK endpoint, which net/smtp allows
	// only because there is no network for it to cross - exactly the
	// "relay on this same host" case, and the one way this test can carry
	// a credential through a real submission conversation at all.
	body.SMTP.Username = "sink-user"
	body.SMTP.Password = testSMTPPassword
	enroll := postJSON(t, client, server.URL+"/api/v1/auth/enroll", body,
		map[string]string{CSRFHeaderName: csrf, BootstrapTokenHeader: token})
	enrollBody, _ := io.ReadAll(enroll.Body)
	enroll.Body.Close()
	if enroll.StatusCode != http.StatusNoContent {
		t.Fatalf("enroll status = %d, want 204; body=%s", enroll.StatusCode, enrollBody)
	}
	waitForMessage(t, sink, "settings", verifySubject)

	test := postJSON(t, client, server.URL+"/api/v1/auth/recovery/test", struct{}{}, map[string]string{CSRFHeaderName: csrf})
	test.Body.Close()
	if test.StatusCode != http.StatusNoContent {
		t.Fatalf("test-send status = %d, want 204", test.StatusCode)
	}
	waitForMessage(t, sink, "settings", testSubject)

	// Nothing that came back over the API, and nothing on disk in the
	// store, may contain the credential.
	read, err := client.Get(server.URL + "/api/v1/auth/recovery")
	if err != nil {
		t.Fatalf("GET recovery: %v", err)
	}
	raw, _ := io.ReadAll(read.Body)
	read.Body.Close()
	if strings.Contains(string(raw), testSMTPPassword) {
		t.Fatalf("GET /recovery returned the SMTP password:\n%s", raw)
	}
	if err := ensureNoPasswordOnDisk(svc, testSMTPPassword); err != nil {
		t.Error(err)
	}
}
