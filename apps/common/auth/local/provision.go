package local

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/retnd/retnd/apps/common/email"
)

// Creating the administrator from the command line, without the browser
// flow.
//
// This exists because the bootstrap-token flow assumes somebody can watch
// the process's log and then reach its port, and an automated deployment
// can do neither. #322 is the issue; the interesting part is why adding it
// does not weaken §49.1.
//
// §49.1 protects against an untrusted network peer claiming an unclaimed
// instance. CreateAdmin is not reachable from the network at all: it needs
// write access to the store path, which is a trust boundary this product
// already hands everything else to, the config file and the state database
// and the SSH keys included. Somebody who has that does not need to steal
// the account, they already own everything the account could reach.
//
// What it does have to get right is the concurrency, because it writes
// straight past Service's read-modify-write cycle. It takes the same
// exclusive lock a running Service holds, and refuses rather than waits.

// CreateAdminConfig is the input to CreateAdmin.
type CreateAdminConfig struct {
	// StorePath is where the administrator record is persisted - the
	// same store.go file a Service constructed with Config.StorePath set
	// to the same value reads and writes.
	StorePath string

	// Username/Password are the credentials to provision. Password is
	// validated against the same minimum handleEnroll enforces
	// (minPasswordLength, handler.go) and is never persisted itself -
	// only hashPassword's output is (password.go).
	Username string
	Password string

	// RecoveryEmail and SMTP are account recovery (#830), and they are
	// OPTIONAL here while being required on the browser's /enroll route.
	//
	// That difference is deliberate and is the same argument this file's
	// opening note already makes about the bootstrap token. The browser
	// flow has an operator in front of it who can be told "your mail
	// server refused this" and can fix it on the spot, so requiring a
	// proven endpoint costs them one retry and buys a recoverable
	// account. This command runs unattended, from a provisioning script,
	// possibly before the host has any route to a mail server at all;
	// requiring SMTP here would mean an automated deployment could not
	// create its administrator, which is the entire reason #322 added
	// this command. So recovery may be left unconfigured and finished
	// later from the Settings page - which is a state the UI reports
	// rather than hides (GET /recovery answers with an empty address and
	// recoveryEmailConfirmed false).
	//
	// When SMTP IS supplied, this command behaves exactly like the
	// browser route: the same VERIFICATION message is sent (#830 §8), a
	// send that fails fails the whole command rather than leaving an
	// account whose recovery address has never received anything, and
	// the account it writes is PROVISIONAL - if nobody opens the link
	// within 30 minutes, the next Service to run deletes it and reopens
	// enrollment (verify.go's reaper). An unattended deployment that
	// provisions an administrator therefore has to be able to receive
	// that message, which is the point: an automated deployment with a
	// wrong recovery address is exactly as locked out as a hand-made one.
	RecoveryEmail string
	SMTP          *email.Config

	// BaseURL is this deployment's externally reachable address, used to
	// build the verification link the message carries - the same
	// Config.BaseURL the server is given (`--public-base-url`). Empty is
	// supported: the message then carries the bare token and names the
	// page to paste it into, because a provisioning script frequently
	// does not know the address operators will reach the deployment at.
	BaseURL string

	// SendMail is the seam the verification send goes through; nil means
	// apps/common/email.Send, exactly like Config.SendMail.
	SendMail email.Sender

	// Now is a seam over time.Now for tests; nil means time.Now, exactly
	// like Config.Now.
	Now func() time.Time
}

// CreateAdmin provisions this package's one administrator record
// (store.go's AdminRecord) directly, without ever going through
// Service.Handler's HTTP /enroll route, a CSRF cookie, or
// bootstrap.go's in-memory, single-use, network-reachable bootstrap
// token (issue #322).
//
// # Why this is safe (issue #322's own security question)
//
// docs/EPIC-B-multi-nas.md §49.1 requires that "reaching the port SHALL
// NOT be sufficient to claim the account" - the bootstrap token exists so
// that whoever happens to connect to an unclaimed instance first cannot
// become its administrator. CreateAdmin does not touch that property at
// all: it has no network listener, accepts no connection, and requires
// nothing but the ability to write to StorePath. Filesystem access to
// StorePath is a DIFFERENT trust boundary than "reaching the port" - one
// this project already grants complete trust to everywhere else
// (config.yaml, the state database, an imported SSH key) - so a caller
// able to invoke CreateAdmin already has, by this product's existing
// threat model, exactly the level of trust §49.1 is protecting the
// account from an untrusted network peer NOT having.
//
// # Concurrency safety
//
// CreateAdmin writes straight into the on-disk file a running Service's
// Store.Enroll/Store.SetPassword read-modify-write cycle also touches
// (store.go's own doc has always assumed "a single process owns path").
// It therefore takes the same exclusive advisory lock Service.New holds
// for its whole lifetime (lock_unix.go/lock_other.go) before writing
// anything, and refuses with ErrStoreLocked if a running server already
// holds it, rather than risking a lost write or a corrupted store. This
// command is meant to run before first start, or while the server is
// stopped: it deliberately does not wait for or coordinate with a live
// one, it fails fast and tells the operator to stop it first.
//
// # Compatibility
//
// The record CreateAdmin writes is produced by exactly the same
// hashPassword (password.go) and exactly the same Store.Enroll
// (store.go, including its own §49.1 single-shot guard) that
// handleEnroll uses - an administrator provisioned this way is
// byte-for-byte indistinguishable, to Store.Admin, handleLogin, or
// anything else that later reads the store, from one who enrolled
// through a browser.
func CreateAdmin(cfg CreateAdminConfig) (*AdminRecord, error) {
	if cfg.StorePath == "" {
		return nil, fmt.Errorf("local: CreateAdminConfig.StorePath is required")
	}
	if cfg.Username == "" {
		return nil, fmt.Errorf("local: CreateAdminConfig.Username is required")
	}
	if len(cfg.Password) < minPasswordLength {
		return nil, fmt.Errorf("local: password must be at least %d characters", minPasswordLength)
	}
	if cfg.RecoveryEmail != "" {
		if err := email.ValidateAddress(cfg.RecoveryEmail); err != nil {
			return nil, fmt.Errorf("local: recovery email: %w", err)
		}
	}
	if cfg.SMTP != nil {
		if err := cfg.SMTP.Validate(); err != nil {
			return nil, fmt.Errorf("local: SMTP configuration: %w", err)
		}
		if cfg.RecoveryEmail == "" {
			// An endpoint with nothing to send to is half a
			// configuration, and the half that is missing is the one
			// recovery actually needs. Refused here rather than stored,
			// because a store carrying SMTP and no address would make
			// the Settings page show a working mail server beside an
			// account that still cannot be recovered.
			return nil, fmt.Errorf("local: an SMTP configuration needs a recovery email to send to")
		}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	sendMail := cfg.SendMail
	if sendMail == nil {
		sendMail = email.Send
	}

	lock, err := acquireStoreLock(cfg.StorePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.release() }()

	hash, err := hashPassword(cfg.Password)
	if err != nil {
		return nil, fmt.Errorf("local: hash password: %w", err)
	}

	createdAt := now().UTC()
	admin := AdminRecord{
		Username:      cfg.Username,
		PasswordHash:  hash,
		RecoveryEmail: cfg.RecoveryEmail,
		CreatedAt:     createdAt,
	}

	var smtp *SMTPRecord
	if cfg.SMTP != nil {
		// The same proof the browser route demands, in the same order:
		// the message goes out BEFORE anything is written, so a command
		// that could not reach the recovery address creates no account.
		//
		// There is no bootstrap token in this process, so the deadline
		// is the floor half of #830 §9's rule on its own
		// (verificationDeadline with a zero window end). The challenge
		// is persisted with the record rather than held in memory,
		// which is what lets the SERVER - a different process, started
		// afterwards - redeem the link this command mailed.
		deadline := verificationDeadline(createdAt, time.Time{})
		challenge, err := mintVerificationChallenge(createdAt)
		if err != nil {
			return nil, fmt.Errorf("local: mint a verification token: %w", err)
		}
		msg := verifyMessage(cfg.RecoveryEmail, cfg.Username, challenge.Token, strings.TrimRight(cfg.BaseURL, "/"), &deadline)
		if err := sendMail(context.Background(), *cfg.SMTP, msg); err != nil {
			return nil, fmt.Errorf("local: sending the verification email: %w", err)
		}
		admin.RecoveryEmailConfirmedAt = &createdAt
		admin.VerificationDeadline = &deadline
		admin.VerificationTokenHash = challenge.Hash
		admin.VerificationTokenExpiresAt = &challenge.ExpiresAt

		passwordRef := ""
		if cfg.SMTP.Password != "" {
			passwordRef, err = newSecretVault(cfg.StorePath).put(cfg.SMTP.Password)
			if err != nil {
				return nil, err
			}
		}
		smtp = &SMTPRecord{
			Host:        cfg.SMTP.Host,
			Port:        cfg.SMTP.Port,
			Security:    string(cfg.SMTP.Security),
			Username:    cfg.SMTP.Username,
			PasswordRef: passwordRef,
			From:        cfg.SMTP.From,
		}
	}

	if err := NewStore(cfg.StorePath).Enroll(admin, smtp); err != nil {
		if smtp != nil {
			newSecretVault(cfg.StorePath).remove(smtp.PasswordRef)
		}
		return nil, err
	}
	return &admin, nil
}
