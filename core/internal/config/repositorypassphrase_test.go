package config

import (
	"strings"
	"testing"
)

// The passphrase a repository is opened with is the one secret that makes
// a repository domain usable, and #783 is where a domain stops being a
// declaration and starts being something this product opens. These are the
// four rules that follow from that, and the asymmetry in the middle two is
// the interesting part: a domain nothing references may omit it, and a
// domain a set names may not.

func TestValidate_ADomainABackupSetNamesMustSayWhereItsPassphraseComesFrom(t *testing.T) {
	c := incrementalConfig()
	c.RepositoryDomains[0].Passphrase = Passphrase{}

	err := c.Validate()
	if err == nil {
		t.Fatal("a repository domain with no passphrase was accepted for a set that stores snapshots in it")
	}

	for _, want := range []string{"production", "passphrase.file", "passphrase.env", "passphrase.command", "encrypted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

func TestValidate_ADomainNothingReferencesNeedsNoPassphrase(t *testing.T) {
	c := incrementalConfig()
	c.RepositoryDomains = append(c.RepositoryDomains, RepositoryDomainConfig{
		ID:        "customer-a",
		Isolation: "isolated",
	})

	if err := c.Validate(); err != nil {
		t.Fatalf("a declared-but-unused domain with no passphrase was refused: %v", err)
	}
}

func TestValidate_ADomainDeclaringTwoPassphraseSourcesIsRefused(t *testing.T) {
	c := incrementalConfig()
	c.RepositoryDomains[0].Passphrase = Passphrase{
		File: "/run/secrets/repo",
		Env:  "BACKUPD_REPO_PASSPHRASE",
	}

	err := c.Validate()
	if err == nil {
		t.Fatal("a domain naming two secret sources was accepted; which one is in force would be whatever the resolver happened to check first")
	}

	if !strings.Contains(err.Error(), "passphrase") {
		t.Errorf("the refusal does not name the key at fault: %v", err)
	}
}

func TestValidate_ResolvesThePassphraseIntoTheReferenceTheEngineTakes(t *testing.T) {
	c := incrementalConfig()
	c.RepositoryDomains[0].Passphrase = Passphrase{File: "/run/secrets/backupd_repo_production"}

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	ref := c.RepositoryDomains[0].PassphraseRef
	if ref.File != "/run/secrets/backupd_repo_production" {
		t.Errorf("PassphraseRef.File = %q, want the declared file", ref.File)
	}

	if err := ref.Validate(); err != nil {
		t.Errorf("the resolved reference is not usable: %v", err)
	}

	// A Ref renders its SOURCE and never its material, which is what
	// makes a repository location safe to log. The command form is the
	// one that could leak an argv, so it is the one checked here.
	c2 := incrementalConfig()
	c2.RepositoryDomains[0].Passphrase = Passphrase{Command: []string{"/usr/bin/vault", "read", "secret/backupd"}}

	if err := c2.Validate(); err != nil {
		t.Fatalf("Validate with a command source: %v", err)
	}

	if rendered := c2.RepositoryDomains[0].PassphraseRef.String(); strings.Contains(rendered, "secret/backupd") {
		t.Errorf("the rendered reference carries the command's arguments: %q", rendered)
	}
}

func TestValidate_IsIdempotentAboutTheResolvedPassphrase(t *testing.T) {
	c := incrementalConfig()

	if err := c.Validate(); err != nil {
		t.Fatalf("first Validate: %v", err)
	}

	first := c.RepositoryDomains[0].PassphraseRef

	if err := c.Validate(); err != nil {
		t.Fatalf("second Validate: %v", err)
	}
	if again := c.RepositoryDomains[0].PassphraseRef; again.File != first.File || again.Env != first.Env {
		t.Errorf("re-validating changed the resolved reference from %s to %s", first, again)
	}
}
