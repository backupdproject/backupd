package local

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/backupdproject/backupd/apps/common/email"
	"github.com/backupdproject/backupd/apps/common/platform/capabilities"

	"github.com/backupdproject/backupd/core/cliecho"
)

// The composition root: everything this package's doc comment lays out,
// assembled into the two things a provider host actually wires up.
//
// New does three things in an order that matters. It takes the store's
// exclusive lock first, before reading or writing anything, so that a
// second server or a racing create-admin is refused rather than
// interleaved. It reads the store to find out whether anybody has enrolled
// yet. And only if nobody has does it mint a bootstrap token, which is
// what makes a restart mid-enrollment invalidate the token the previous
// run printed.
//
// The lock is never released. There is no Close on Service and that is
// deliberate rather than an omission: the process that holds it is the
// server, it holds the store open for its whole life, and the kernel
// releases the flock when it exits by any means including a kill. An
// explicit release would only add a path where the lock is dropped while
// the store is still in use.
//
// Config.TrustForwardedHeaders is the one setting here with a real blast
// radius, and its own doc carries the topology argument rather than this
// opener, because that is where somebody about to set it will be looking.

// Config is everything New needs to build a Service.
type Config struct {
	// StorePath is where the administrator record is persisted (store.go).
	// Its parent directory is created on first write if missing.
	StorePath string

	// Now is a seam over time.Now for tests; nil means time.Now.
	Now func() time.Time

	// LoginRateLimit/EnrollRateLimit bound login and enrollment attempts
	// per remote IP per minute. Zero means DefaultLoginRateLimit /
	// DefaultEnrollRateLimit.
	LoginRateLimit  int
	EnrollRateLimit int

	// PasswordRateLimit bounds POST /password attempts per remote IP per
	// minute, the same per-IP rate-limit treatment /login already gets
	// (issue #128). Zero means DefaultPasswordRateLimit.
	PasswordRateLimit int

	// RecoveryRateLimit bounds POST /forgot-password and POST
	// /reset-password attempts per remote IP per minute. Zero means
	// DefaultRecoveryRateLimit.
	//
	// It is tighter than the others on purpose (#830): forgot-password
	// makes this process SEND something, to an address the caller does
	// not get to choose, so an unbounded one is a way to flood the
	// administrator's mailbox and burn the operator's SMTP quota using
	// nothing but an open port.
	RecoveryRateLimit int

	// BaseURL is this deployment's own externally reachable address
	// ("https://nas.example.com:8080"), used to build the reset link the
	// forgotten-password email carries.
	//
	// Empty is a supported state rather than a misconfiguration: this
	// process genuinely cannot know its own public address (see
	// PrintBootstrapNotice, which has the same problem with the
	// enrollment link), and a link built from a guess would be
	// confidently wrong. With it empty the reset email carries the bare
	// token and tells the operator where to paste it.
	BaseURL string

	// SendMail is the seam every message this package sends goes
	// through; nil means apps/common/email.Send, which is what production
	// wires. A test supplies its own to prove what a handler does with a
	// send that fails, without standing up a mail server for it.
	SendMail email.Sender

	// Log is where the diagnostics for failures that must NOT reach the
	// caller are written - the forgot-password branches, which all answer
	// 204 whatever happened (recovery.go explains why). nil means
	// os.Stderr; io.Discard silences them.
	Log io.Writer

	// Notice is where an operator-facing enrollment notice is written
	// when this Service reopens enrollment BY ITSELF - the reaper
	// deleting a provisional administrator whose recovery address was
	// never verified (verify.go, #830 §9). nil means os.Stdout, which
	// is the stream a host's own startup PrintBootstrapNotice call
	// already writes to (apps/generic/cmd/backupd-web), and the two have
	// to be the same stream for the notice to be findable at all.
	//
	// It is separate from Log because the two are different kinds of
	// line for different readers. Log carries diagnostics nobody is
	// required to read; this carries the single-use token that is the
	// ONLY way back into a deployment whose administrator was just
	// deleted. §49.1 puts that token in the container log, and a reap
	// that minted one where nothing printed it would leave an operator
	// with a reopened enrollment they have 30 minutes to find and no way
	// to see - which is exactly what docs/recovery-without-a-terminal.md
	// promises does not happen.
	Notice io.Writer

	// TrustForwardedHeaders makes this Service trust X-Forwarded-For (for
	// rate limiting, ratelimit.go's remoteIP) and X-Forwarded-Proto (for
	// the session/CSRF cookies' Secure flag, forwarded.go's
	// requestIsSecure) instead of a request's own RemoteAddr/TLS.
	//
	// # Safe ONLY behind one specific, network-isolated reverse proxy
	//
	// Both headers are ordinary request headers: anyone who can reach
	// this Service's handler directly can set either to whatever they
	// like, which would let them pick their own rate-limit bucket (an
	// attacker rotating a fake X-Forwarded-For on every request to evade
	// the limiter entirely) or falsely claim a plaintext connection is
	// HTTPS. Enable this ONLY when this Service's handler is reachable
	// exclusively through one specific reverse proxy that (a) is the sole
	// possible direct TCP peer, by network topology, not merely by
	// convention, and (b) always sets both headers itself, derived from
	// its own real connection to the actual client, never copied from
	// whatever the client sent it.
	//
	// apps/generic's two-container split (container/compose.yaml) is
	// exactly that shape: the engine (this Service, wired by `serve`) has
	// no published port and joins no network but `internal`, which only
	// `web-ui` (`serve-ui`, apps/common/webhost/serve.NewUI's reverse proxy)
	// also joins - nothing else on the host, and nothing on the LAN, can
	// ever be this Service's direct peer. apps/generic/cmd/backupd-web's
	// `--trust-forwarded-headers` flag is what actually turns this on for
	// that deployment; container/compose.yaml sets it for the
	// `backupd` (engine) service only, never for `web-ui` itself
	// (which correctly observes its own real TLS status directly and must
	// never trust a forwarded header from just anyone hitting its
	// published port).
	//
	// Defaults to false: a Service instantiated without this set (every
	// test in this package, and any future caller that doesn't know its
	// own topology guarantees this) trusts nothing but its own directly
	// observed connection, which is always safe regardless of topology.
	TrustForwardedHeaders bool

	// ReapInterval is how often the background reaper asks whether the
	// provisional administrator's verification deadline has passed
	// (verify.go, #830 §9). Zero means DefaultReapInterval.
	//
	// It is a real-time ticker even when Now is faked, because it
	// decides how often to ASK and Now decides the answer. A test that
	// wants the decision without waiting calls it directly; one that
	// wants to prove the timer itself fires sets a small interval here
	// and moves Now.
	ReapInterval time.Duration
}

// Default rate limits: generous enough that an operator mistyping a
// password a few times in a row is never the thing that trips this, tight
// enough to make an automated guessing loop impractical.
const (
	DefaultLoginRateLimit    = 10
	DefaultEnrollRateLimit   = 5
	DefaultPasswordRateLimit = 10
	// DefaultRecoveryRateLimit bounds forgot-password and reset-password.
	// Lower than the rest because the first of the two makes this process
	// send mail on an unauthenticated request (Config.RecoveryRateLimit).
	DefaultRecoveryRateLimit = 3
	rateLimitWindow          = time.Minute
)

// Service composes everything this package's doc comment describes into
// the one thing a provider host actually wires up: an Authenticator for
// apps/common/webhost's authMiddleware, and an http.Handler for the
// login/enroll/logout/session and account-recovery routes themselves.
type Service struct {
	store                 *Store
	lock                  *storeLock
	secrets               *secretVault
	sessions              *sessionManager
	bootstrap             *singleUseIssuer
	resetTokens           *singleUseIssuer
	loginLimiter          *RateLimiter
	enrollLimiter         *RateLimiter
	rotateLimiter         *RateLimiter
	forgotLimiter         *RateLimiter
	resetLimiter          *RateLimiter
	verifyLimiter         *RateLimiter
	sendMail              email.Sender
	baseURL               string
	log                   io.Writer
	notice                io.Writer
	outbound              sync.WaitGroup
	now                   func() time.Time
	trustForwardedHeaders bool
	reapInterval          time.Duration
	reaperStop            chan struct{}
	reaperStopOnce        sync.Once
}

// New builds a Service from cfg.
//
// Three things happen in an order that matters. It takes the store's
// exclusive lock. It REAPS a provisional administrator whose recovery
// address was never verified before its deadline (verify.go, #830 §9),
// which is why a process that was down through an entire verification
// window still cleans up on its next start. And only then does it ask
// whether anybody is enrolled: if nobody is - including because the reap
// just deleted the record - it issues a fresh bootstrap token
// immediately (see PrintBootstrapNotice to actually surface it to the
// operator), so a process restart before enrollment completes always
// invalidates whatever token a previous run may have printed (§49.1:
// "single-use and SHALL expire").
//
// It also starts the background reaper, which runs for this Service's
// whole life. There is deliberately no public way to stop it, for the
// reason there is no Close: the process that owns a Service owns it
// until it exits.
func New(cfg Config) (*Service, error) {
	if cfg.StorePath == "" {
		return nil, fmt.Errorf("local: Config.StorePath is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	loginLimit := cfg.LoginRateLimit
	if loginLimit == 0 {
		loginLimit = DefaultLoginRateLimit
	}
	enrollLimit := cfg.EnrollRateLimit
	if enrollLimit == 0 {
		enrollLimit = DefaultEnrollRateLimit
	}
	passwordLimit := cfg.PasswordRateLimit
	if passwordLimit == 0 {
		passwordLimit = DefaultPasswordRateLimit
	}
	recoveryLimit := cfg.RecoveryRateLimit
	if recoveryLimit == 0 {
		recoveryLimit = DefaultRecoveryRateLimit
	}
	reapInterval := cfg.ReapInterval
	if reapInterval == 0 {
		reapInterval = DefaultReapInterval
	}
	sendMail := cfg.SendMail
	if sendMail == nil {
		sendMail = email.Send
	}
	log := cfg.Log
	if log == nil {
		log = os.Stderr
	}
	notice := cfg.Notice
	if notice == nil {
		notice = os.Stdout
	}

	// Take this store's exclusive advisory lock BEFORE reading or writing
	// anything, and hold it for this Service's entire lifetime (there is
	// deliberately no Close/release - the real, long-lived `serve`
	// process holds it until it exits, at which point the kernel releases
	// it automatically, exactly like core/service/lock_unix.go's own
	// startup/journal locks). This is what makes store.go's own
	// documented assumption - "a single process owns path" - an enforced
	// invariant rather than a convention: a second `serve` against the
	// same store, or a `create-admin` invocation (provision.go) racing a
	// live one, is refused with ErrStoreLocked instead of racing this
	// Service's own Store.Enroll/Store.SetPassword read-modify-write
	// cycle (issue #322).
	lock, err := acquireStoreLock(cfg.StorePath)
	if err != nil {
		return nil, err
	}

	store := NewStore(cfg.StorePath)

	loginLimiter := NewRateLimiter(loginLimit, rateLimitWindow)
	loginLimiter.now = now
	enrollLimiter := NewRateLimiter(enrollLimit, rateLimitWindow)
	enrollLimiter.now = now
	rotateLimiter := NewRateLimiter(passwordLimit, rateLimitWindow)
	rotateLimiter.now = now
	forgotLimiter := NewRateLimiter(recoveryLimit, rateLimitWindow)
	forgotLimiter.now = now
	resetLimiter := NewRateLimiter(recoveryLimit, rateLimitWindow)
	resetLimiter.now = now
	verifyLimiter := NewRateLimiter(recoveryLimit, rateLimitWindow)
	verifyLimiter.now = now

	s := &Service{
		store:                 store,
		lock:                  lock,
		secrets:               newSecretVault(cfg.StorePath),
		sessions:              newSessionManager(now),
		bootstrap:             newBootstrapIssuer(now),
		resetTokens:           newResetTokenIssuer(now),
		loginLimiter:          loginLimiter,
		enrollLimiter:         enrollLimiter,
		rotateLimiter:         rotateLimiter,
		forgotLimiter:         forgotLimiter,
		resetLimiter:          resetLimiter,
		verifyLimiter:         verifyLimiter,
		sendMail:              sendMail,
		baseURL:               strings.TrimRight(cfg.BaseURL, "/"),
		log:                   log,
		notice:                notice,
		now:                   now,
		trustForwardedHeaders: cfg.TrustForwardedHeaders,
		reapInterval:          reapInterval,
		reaperStop:            make(chan struct{}),
	}

	// Before the enrollment question below, so that a lapsed provisional
	// administrator is gone by the time it is asked and the fresh
	// bootstrap token this start prints is the one enrollment will use.
	if _, err := s.reapUnverifiedAdmin(); err != nil {
		_ = lock.release()
		return nil, fmt.Errorf("local: reap an unverified administrator: %w", err)
	}

	admin, err := store.Admin()
	if err != nil {
		_ = lock.release()
		return nil, fmt.Errorf("local: read existing administrator: %w", err)
	}
	if admin == nil {
		if _, err := s.bootstrap.issue(); err != nil {
			_ = lock.release()
			return nil, fmt.Errorf("local: issue bootstrap token: %w", err)
		}
	}

	s.startReaper()
	return s, nil
}

// Authenticator returns the capabilities.Authenticator apps/common/webhost's
// authMiddleware should consult for /api/v1 requests.
func (s *Service) Authenticator() capabilities.Authenticator {
	return sessionAuthenticator{sessions: s.sessions}
}

// TrustForwardedHeaders reports whether this Service was configured to
// trust X-Forwarded-For/X-Forwarded-Proto from its immediate caller (see
// Config.TrustForwardedHeaders's own doc for exactly when that is safe).
// apps/generic/cmd/backupd-web calls this to fill
// apps/common/webhost/serve.EngineConfig.TrustForwardedHeaders, which
// decides the same thing for the CSRF cookie NewEngine issues
// (EnsureCSRFCookie) that this Service's own session cookie already
// decides for itself internally (handler.go), so both are governed by
// the one Config value a caller actually set, instead of two
// independently-configured, driftable settings.
func (s *Service) TrustForwardedHeaders() bool {
	return s.trustForwardedHeaders
}

// NeedsEnrollment reports whether no administrator has been created yet.
func (s *Service) NeedsEnrollment() (bool, error) {
	admin, err := s.store.Admin()
	if err != nil {
		return false, err
	}
	return admin == nil, nil
}

// PrintBootstrapNotice writes the current bootstrap token's enrollment
// instructions to w, if (and only if) enrollment is still open - it does
// nothing once an administrator exists, so calling it unconditionally at
// startup is always safe. baseURL (e.g. "http://localhost:8080") is used
// to print a direct link an operator can open; pass "" to print just the
// bare token instead.
//
// §49.1's "printed to the container log on first start" is what this
// method is for: a caller (the generic host's serve command) calls it
// once, right after constructing the Service, against the process's own
// stdout - the container's own log, exactly as the spec names it.
//
// That is also why the line is prefixed with the web host's own name
// rather than this package's. It lands in the same log as every other
// line the process writes, and it is usually the FIRST line a new
// deployment shows anybody, so a prefix nothing else in the image uses is
// the worst one to have. It said `backupd` until 0.3.3, which was
// already the wrong half of the pair (the CLI never prints this), and
// the rename made it a name the product no longer answers to at all. It
// reads core/cliecho.WebBinary now, the one place that name is spelled.
//
// The web host is the only caller today. Should a second provider host
// grow one, the prefix becomes a Config field rather than a constant
// here: a shared package hard-coding one caller's name is exactly the
// drift this is fixing, and it is worth saying that out loud before
// somebody copies the pattern.
func (s *Service) PrintBootstrapNotice(w io.Writer, baseURL string) error {
	needsEnrollment, err := s.NeedsEnrollment()
	if err != nil {
		return err
	}
	if !needsEnrollment {
		return nil
	}

	s.bootstrap.mu.Lock()
	token := ""
	if s.bootstrap.token != nil && !s.bootstrap.token.used {
		token = s.bootstrap.token.value
	}
	s.bootstrap.mu.Unlock()
	if token == "" {
		return nil
	}

	if baseURL != "" {
		_, err = fmt.Fprintf(w,
			cliecho.WebBinary+": no administrator account exists yet. Open %s/enroll?token=%s to create one (valid 30 minutes, single use).\n",
			baseURL, token)
	} else {
		_, err = fmt.Fprintf(w,
			cliecho.WebBinary+": no administrator account exists yet. Enrollment bootstrap token: %s (valid 30 minutes, single use).\n",
			token)
	}
	return err
}
