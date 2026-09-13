package local

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"sync"
	"time"
)

// The single-use secrets this package hands out: the one that stands
// between an unclaimed deployment and whoever reaches its port first, and
// the one that stands between a forgotten password and a new one.
//
// §49.1's requirement for the first is easy to state and easy to get
// subtly wrong: reaching the port must not be enough to claim the
// administrator account. The token this file issues is what makes the
// difference, and its three properties all exist because of a specific
// way the naive version leaks. It is held in memory only, so a restart
// before enrollment completes invalidates whatever a previous run printed
// rather than leaving a standing credential in a file somebody might
// later read. There is only ever one live at a time, so a second start
// cannot leave two valid claims. And consume marks it used inside the
// same lock that checks it, so two requests arriving together cannot both
// win.
//
// The bootstrap token is printed to the process's own stdout and nowhere
// else. That is not a limitation to work around later: any channel that
// could deliver it elsewhere would be a channel an attacker could try to
// reach.
//
// The password-reset token (#830) is the same mechanism with a different
// delivery channel and a different TTL, which is why it is the same type
// rather than a second copy of it. It is delivered by email to the
// administrator's confirmed recovery address, and it is held in memory
// for the same reason: a restart invalidating an outstanding reset link
// is the correct behaviour for a credential that was mailed out, and a
// persisted one would be a standing password-reset credential sitting in
// the state directory.

// bootstrapTokenTTL bounds how long a printed enrollment token remains
// valid (§49.1: "SHALL be single-use and SHALL expire"). 30 minutes is
// long enough for an operator to read the container's own startup log
// and paste the token into the enrollment page, short enough that a
// token nobody used stops being a standing credential.
const bootstrapTokenTTL = 30 * time.Minute

// resetTokenTTL bounds how long an emailed password-reset link remains
// valid. The same 30 minutes as the bootstrap token, for the same reason
// and deliberately not longer: the operator who asked for the link is
// sitting in front of their mail client, and a link that stays live for
// hours is a password-reset credential lying in an inbox.
const resetTokenTTL = 30 * time.Minute

type singleUseToken struct {
	value     string
	expiresAt time.Time
	used      bool
}

// singleUseIssuer hands out one live, expiring, single-use token at a
// time. Issuing a new one silently invalidates whatever token came before
// it, which is exactly what should happen both across a process restart
// before enrollment completes and when an operator asks for a second
// password-reset link.
type singleUseIssuer struct {
	mu    sync.Mutex
	token *singleUseToken
	now   func() time.Time
	ttl   time.Duration
}

// newBootstrapIssuer returns the issuer for the enrollment bootstrap
// token §49.1 requires before enrollment can claim the administrator
// account: "reaching the port SHALL NOT be sufficient to claim the
// account." Service issues one, at construction, only when no
// administrator exists yet (service.go).
func newBootstrapIssuer(now func() time.Time) *singleUseIssuer {
	return &singleUseIssuer{now: now, ttl: bootstrapTokenTTL}
}

// newResetTokenIssuer returns the issuer for the password-reset token
// emailed by POST /forgot-password and redeemed by POST /reset-password
// (#830).
func newResetTokenIssuer(now func() time.Time) *singleUseIssuer {
	return &singleUseIssuer{now: now, ttl: resetTokenTTL}
}

// issue generates and remembers a fresh token, replacing any previous
// one, and returns its value.
func (b *singleUseIssuer) issue() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("local: generate single-use token: %w", err)
	}
	value := base64.RawURLEncoding.EncodeToString(raw)

	b.mu.Lock()
	b.token = &singleUseToken{value: value, expiresAt: b.now().Add(b.ttl)}
	b.mu.Unlock()
	return value, nil
}

// valid reports whether candidate is the current, unexpired, unused token
// WITHOUT spending it.
//
// It exists for one specific ordering (#830). Enrollment now has work to
// do between authenticating the caller and committing anything: it sends
// a confirmation message over the SMTP endpoint the request carried, and
// that send is allowed to fail. Spending the token first would mean a
// mistyped SMTP password burned the operator's only enrollment link and
// left them needing a process restart to get another; checking here and
// spending it in consume once the send has succeeded means a refusal
// costs nothing but a retry. The token is still never accepted twice -
// consume is what decides that, atomically, and it is what runs before
// the record is written.
func (b *singleUseIssuer) valid(candidate string) bool {
	if candidate == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.currentMatches(candidate)
}

// consume reports whether candidate is the current, unexpired, unused
// token, and if so marks it used atomically with that check - it can
// never succeed twice for the same token, and a comparison against the
// stored value runs in constant time so a mismatch cannot be
// distinguished by timing.
func (b *singleUseIssuer) consume(candidate string) bool {
	if candidate == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.currentMatches(candidate) {
		return false
	}
	b.token.used = true
	return true
}

// currentMatches is valid/consume's shared decision. Callers hold b.mu.
func (b *singleUseIssuer) currentMatches(candidate string) bool {
	if b.token == nil || b.token.used {
		return false
	}
	if b.now().After(b.token.expiresAt) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(b.token.value), []byte(candidate)) == 1
}
