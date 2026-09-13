// Issue #845's two scopes for the source poll interval: the
// deployment-wide default, and a backup set's own override of it.
//
// The rules under test are the ones a daemon cadence is built out of --
// what an omitted per-set key means, what the floor is, and which value a
// given set actually polls on -- because every one of them is a rule
// something outside this package would otherwise have to re-derive.

package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestBackupSetPollIntervalIsAbsentUnlessWritten(t *testing.T) {
	doc := "poll_interval: 15m\n" +
		"state:\n  database: /var/lib/backupd/state.db\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres\n" +
		"        remote:\n          type: sftp\n          host: h\n          port: 22\n          user: u\n          key_file: /k\n          known_hosts: /kh\n" +
		"        remote_path: /r\n        local_path: /l\n        include: [\"*.zst\"]\n" +
		"        completion:\n          strategy: rename\n" +
		"        stale_after: 30h\n" +
		"        validation:\n          hash: sha256\n"

	var cfg Config
	if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := cfg.Sources[0].BackupSets[0].PollInterval; got != nil {
		t.Fatalf("PollInterval = %s, want nil: a set that wrote no poll_interval inherits the deployment's", got)
	}

	withOverride := strings.Replace(doc, "        stale_after: 30h\n", "        stale_after: 30h\n        poll_interval: 5m\n", 1)
	var overridden Config
	if err := yaml.Unmarshal([]byte(withOverride), &overridden); err != nil {
		t.Fatalf("unmarshal override: %v", err)
	}
	got := overridden.Sources[0].BackupSets[0].PollInterval
	if got == nil || got.Duration() != 5*time.Minute {
		t.Fatalf("PollInterval = %v, want 5m", got)
	}
}

// TestBackupSetPollIntervalRoundTripsOnlyWhenWritten is FR-35's round
// trip: core/service re-marshals the whole Config on every settings save,
// and a file that never named a per-set interval must not come back from
// one carrying the key, which an older build refuses outright under
// Load's KnownFields(true).
func TestBackupSetPollIntervalRoundTripsOnlyWhenWritten(t *testing.T) {
	cfg := validConfig()
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "poll_interval: 15m\n      ") || strings.Count(string(out), "poll_interval") != 1 {
		t.Fatalf("a set that configured no poll_interval gained the key:\n%s", out)
	}

	five := Duration(5 * time.Minute)
	cfg.Sources[0].BackupSets[0].PollInterval = &five
	out, err = yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Count(string(out), "poll_interval") != 2 {
		t.Fatalf("a set that configured a poll_interval lost it:\n%s", out)
	}
}

func TestPollIntervalFloor(t *testing.T) {
	t.Run("global below the floor is refused", func(t *testing.T) {
		cfg := validConfig()
		cfg.PollInterval = Duration(30 * time.Second)
		err := cfg.Validate()
		if err == nil {
			t.Fatal("a 30s poll_interval was accepted")
		}
		if !strings.Contains(err.Error(), MinPollInterval.String()) {
			t.Errorf("refusal does not name the floor: %v", err)
		}
	})

	t.Run("the floor itself is accepted", func(t *testing.T) {
		cfg := validConfig()
		cfg.PollInterval = Duration(MinPollInterval)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("the floor itself was refused: %v", err)
		}
	})

	t.Run("a per-set override below the floor is refused", func(t *testing.T) {
		cfg := validConfig()
		d := Duration(10 * time.Second)
		cfg.Sources[0].BackupSets[0].PollInterval = &d
		err := cfg.Validate()
		if err == nil {
			t.Fatal("a 10s per-set poll_interval was accepted")
		}
		if !strings.Contains(err.Error(), "sources[0].backup_sets[0]: poll_interval") {
			t.Errorf("refusal does not name the field: %v", err)
		}
	})

	t.Run("a non-positive per-set override is refused", func(t *testing.T) {
		for _, d := range []Duration{0, Duration(-time.Hour)} {
			cfg := validConfig()
			cfg.Sources[0].BackupSets[0].PollInterval = &d
			if err := cfg.Validate(); err == nil {
				t.Fatalf("a per-set poll_interval of %s was accepted", d)
			}
		}
	})

	t.Run("a per-set override at or above the floor is accepted", func(t *testing.T) {
		cfg := validConfig()
		d := Duration(6 * time.Hour)
		cfg.Sources[0].BackupSets[0].PollInterval = &d
		if err := cfg.Validate(); err != nil {
			t.Fatalf("a 6h per-set poll_interval was refused: %v", err)
		}
	})
}

func TestEffectivePollInterval(t *testing.T) {
	cfg := validConfig()
	cfg.PollInterval = Duration(15 * time.Minute)
	inherits := cfg.Sources[0].BackupSets[0]
	if got, want := cfg.EffectivePollInterval(inherits), 15*time.Minute; got != want {
		t.Errorf("EffectivePollInterval(no override) = %s, want %s", got, want)
	}

	d := Duration(2 * time.Hour)
	overridden := inherits
	overridden.PollInterval = &d
	if got, want := cfg.EffectivePollInterval(overridden), 2*time.Hour; got != want {
		t.Errorf("EffectivePollInterval(override) = %s, want %s", got, want)
	}
}
