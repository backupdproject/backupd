package compat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/retnd/retnd/core/internal/state"
)

// FR-38's four cells, captured from the real binary against real
// directories (EPIC R, #890).
//
// WHY THIS IS A COMPAT CELL AND NOT A UNIT TEST. The thing under test is
// not the classifier — that is a pure function over two stat results and
// core/legacypath is where it lives. It is the WORDING an operator meets:
// a warning printed on every start of a deployment that was upgraded
// across the rename, and a refusal printed instead of starting when two
// journals are visible. An operator meets either one once, in an incident,
// months after the upgrade that caused it, and reads whatever it says
// today. That is exactly what this package pins.
//
// The three cells are named by the epic rather than invented here
// (docs/EPIC-R-rename-backupd-to-retnd.md FR-38,
// docs/conformance/epic-r-matrix.md rows R1.9/R2.9/V.1/V.5):
//
//	20-legacy-state-adoption    the second row, BOTH halves: a
//	                            configuration adopted from the
//	                            pre-rename directory, and a journal
//	                            adopted from the pre-rename one. Asserted
//	                            separately because the two are resolved
//	                            by different code (V.1), and carrying the
//	                            reported adopted path with them (V.5).
//	21-fresh-install-first-run  the third row, and the POSITIVE CONTROL
//	                            for cell 20: a genuinely empty
//	                            deployment still reports "no
//	                            configuration", so "it adopted the legacy
//	                            path" cannot be satisfied by a product
//	                            that can no longer be installed at all.
//	22-two-journals-refusal     the fourth row AND the first row's
//	                            same-directory case: two different
//	                            populated directories are refused with
//	                            both named, and ONE directory reachable
//	                            under both names is not refused. The
//	                            second half is the control that keeps
//	                            "it refused the ambiguous case" from
//	                            being satisfiable by a build that refuses
//	                            whenever both paths are populated —
//	                            which would stop the installer's own
//	                            rollback-window compose file from
//	                            starting.
//
// WHAT DRIVES THEM. `retnd check`, the real binary, as a real process:
// it is one of the three surfaces FR-38 requires the adopted path on, it
// goes through exactly the preflight `serve` goes through
// (core/internal/app.Check and core/service.OpenConfigAndJournal call the
// same pure functions), and it is reachable from this module. The other
// two surfaces are the startup log and the deployment-check route, which
// belong to the web host: apps/generic/cmd/retnd-web's own
// TestServe_FR38* cases drive those as real processes, including the two
// facts this module cannot observe — that an adopted start creates no
// administrator record and mints no enrollment token, and that a
// genuinely empty one still serves the first-run flow.
//
// THE PATHS ARE UNDER A THROWAWAY ROOT, not /etc and /var/lib. That is
// not a weakening: core/legacypath decides on a path COMPONENT equal to
// this product's name rather than on two absolute prefixes, precisely so
// that the rule covers an operator who mounts their state at
// /volume1/retnd as well as the packaged /var/lib/retnd, and so that it
// can be driven end to end by a real process against real directories
// instead of by mocking the thing under test. <ROOT>/etc/retnd and
// <ROOT>/etc/backupd are the same pair of names the packaged deployment
// uses, one directory further down.

// identityScenario is one deployment shape to build and run `check`
// against.
type identityScenario struct {
	label string

	// legacyConfig and renamedConfig say whether a config.yaml is
	// planted under <root>/etc/backupd/config and
	// <root>/etc/retnd/config.
	legacyConfig  bool
	renamedConfig bool

	// legacyState and renamedState say whether a real, migrated SQLite
	// journal is planted under <root>/var/lib/backupd and
	// <root>/var/lib/retnd. The configuration written at each config
	// path names the journal in the MATCHING family, so a scenario with
	// a renamed configuration and a legacy journal is the state half of
	// the second row rather than an accident.
	legacyState  bool
	renamedState bool

	// oneDirectoryUnderBothNames replaces the four directories with
	// symlinks into a single host directory, which is the shape the
	// installer's rollback-window compose override produces: one host
	// directory mounted at both container paths. Both spellings then
	// hold a configuration and a journal, and both are the same device
	// and inode.
	oneDirectoryUnderBothNames bool
}

// captureDeploymentIdentity builds each shape, runs `check` against the
// path THIS RELEASE resolves (always the renamed spelling, because that is
// what an upgraded binary does), and returns the three cells.
func captureDeploymentIdentity(ctx context.Context, bin, workDir string) (Cell, Cell, Cell, error) {
	adoption := []identityScenario{
		{
			label:        "a configuration at the pre-rename path and nothing at the renamed one",
			legacyConfig: true, legacyState: true,
		},
		{
			label:         "a configuration at the renamed path naming a journal that is not there, beside a pre-rename one that is",
			renamedConfig: true, legacyState: true,
		},
	}
	fresh := []identityScenario{
		{label: "nothing at either path"},
		{
			label:         "a configuration and a journal at the renamed paths, nothing at the pre-rename ones",
			renamedConfig: true, renamedState: true,
		},
	}
	refusal := []identityScenario{
		{
			label:        "a different configuration at each path",
			legacyConfig: true, renamedConfig: true, legacyState: true, renamedState: true,
		},
		{
			// The cell's namesake: two JOURNALS, refused by the state
			// half rather than by the configuration half. Only the
			// renamed configuration exists here, so the configuration
			// half is unambiguous and the refusal can only come from
			// the journal pair — which is the assertion, because the
			// two halves are decided by different code and a cell that
			// only ever reached the first would say nothing about the
			// second.
			label:         "one configuration naming a journal that exists, beside a different pre-rename journal",
			renamedConfig: true, renamedState: true, legacyState: true,
		},
		{
			label:                      "one host directory reachable under both names (the installer's rollback window)",
			oneDirectoryUnderBothNames: true,
		},
	}

	run := func(scenarios []identityScenario) ([]string, error) {
		var lines []string
		for _, sc := range scenarios {
			root, cfgPath, err := buildIdentityScenario(ctx, workDir, sc)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", sc.label, err)
			}
			lines = append(lines, "# "+sc.label)
			out, err := runCLI(ctx, bin, []string{"check", "--config", cfgPath}, root)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", sc.label, err)
			}
			lines = append(lines, out...)
		}
		return lines, nil
	}

	adoptionLines, err := run(adoption)
	if err != nil {
		return Cell{}, Cell{}, Cell{}, err
	}
	freshLines, err := run(fresh)
	if err != nil {
		return Cell{}, Cell{}, Cell{}, err
	}
	refusalLines, err := run(refusal)
	if err != nil {
		return Cell{}, Cell{}, Cell{}, err
	}

	return Cell{
		Certifies: "FR-38 row 2, both halves, asserted separately because different code resolves them (matrix V.1): a resolved configuration path that holds nothing beside a pre-rename one that does is an ADOPTION, not a fresh install, and so is a resolved journal path that holds nothing beside a pre-rename one that does. The adopted path is reported (matrix V.5), and the warning names the compose line to change and the installer command that changes it. Adoption is a shim with a closing date (#895): the issue that closes the window replaces this cell with one pinning a refusal.",
		Rule:      RuleIdentical,
		Lines:     adoptionLines,
	}, Cell{
		Certifies: "FR-38 row 3, and the positive control for cell 20: a deployment with nothing at either path still reports that it has no configuration, and one whose paths are the ones this release resolves reports no adoption at all. Without this cell, \"it adopted the pre-rename path\" would be satisfiable by a build that adopts unconditionally, or that can no longer be installed fresh at all.",
		Rule:      RuleIdentical,
		Lines:     freshLines,
	}, Cell{
		Certifies: "FR-38 row 4 and the same-directory half of row 1: two different populated directories are REFUSED, naming both, because choosing one silently is the worst option available; and one host directory reachable under both names — which is exactly what the installer's rollback-window compose override produces — is NOT refused, because it is one device and one inode and therefore not ambiguity. Dropping the same-device-and-inode test turns the second case red, which is what makes it a control rather than a restatement.",
		Rule:      RuleIdentical,
		Lines:     refusalLines,
	}, nil
}

// buildIdentityScenario materializes one shape under its own throwaway
// root and returns the root plus the configuration path THIS RELEASE
// resolves — always the renamed spelling, because the whole failure FR-38
// exists to stop starts with an upgraded binary resolving the renamed path
// on a deployment that is still mounted at the pre-rename one.
func buildIdentityScenario(ctx context.Context, workDir string, sc identityScenario) (string, string, error) {
	root := filepath.Join(workDir, "fr38", sanitizeLabel(sc.label))
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", "", err
	}

	renamedConfigDir := filepath.Join(root, "etc", "retnd", "config")
	legacyConfigDir := filepath.Join(root, "etc", "backupd", "config")
	renamedStateDir := filepath.Join(root, "var", "lib", "retnd")
	legacyStateDir := filepath.Join(root, "var", "lib", "backupd")

	if sc.oneDirectoryUnderBothNames {
		// One host directory, four names. This is the installer's
		// rollback-window override written out as symlinks rather than
		// as bind mounts: a bind mount of one host directory at two
		// container paths gives the two paths one device and one inode,
		// and so does this, which is the property under test.
		host := filepath.Join(root, "host")
		if err := os.MkdirAll(filepath.Join(host, "config"), 0o755); err != nil {
			return "", "", err
		}
		if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
			return "", "", err
		}
		if err := os.MkdirAll(filepath.Join(root, "var", "lib"), 0o755); err != nil {
			return "", "", err
		}
		for _, link := range []string{
			filepath.Join(root, "etc", "retnd"),
			filepath.Join(root, "etc", "backupd"),
			filepath.Join(root, "var", "lib", "retnd"),
			filepath.Join(root, "var", "lib", "backupd"),
		} {
			if err := os.RemoveAll(link); err != nil {
				return "", "", err
			}
			if err := os.Symlink(host, link); err != nil {
				return "", "", err
			}
		}
		if err := seedIdentityJournal(ctx, filepath.Join(host, "state.db")); err != nil {
			return "", "", err
		}
		if err := writeIdentityConfig(filepath.Join(host, "config", "config.yaml"), filepath.Join(renamedStateDir, "state.db"), root); err != nil {
			return "", "", err
		}
		return root, filepath.Join(renamedConfigDir, "config.yaml"), nil
	}

	if sc.legacyState {
		if err := seedIdentityJournal(ctx, filepath.Join(legacyStateDir, "state.db")); err != nil {
			return "", "", err
		}
	}
	if sc.renamedState {
		if err := seedIdentityJournal(ctx, filepath.Join(renamedStateDir, "state.db")); err != nil {
			return "", "", err
		}
	}
	// Each configuration names the journal in its OWN family. That is
	// what makes the second adoption case the state half of row 2 rather
	// than a configuration pointing at a path nobody planted: a
	// deployment whose migration rewrote the persisted path and did not
	// move the mount.
	if sc.legacyConfig {
		if err := writeIdentityConfig(filepath.Join(legacyConfigDir, "config.yaml"), filepath.Join(legacyStateDir, "state.db"), root); err != nil {
			return "", "", err
		}
	}
	if sc.renamedConfig {
		if err := writeIdentityConfig(filepath.Join(renamedConfigDir, "config.yaml"), filepath.Join(renamedStateDir, "state.db"), root); err != nil {
			return "", "", err
		}
	}
	return root, filepath.Join(renamedConfigDir, "config.yaml"), nil
}

// seedIdentityJournal plants a REAL migrated journal, through the same
// state.Open a deployment uses.
//
// A hand-written file with SQLite's magic bytes would satisfy the
// preflight — which only asks whether the path holds a regular file with
// bytes in it — and then fail the open a moment later, so the cell would
// pin a different failure from the one it claims to be about.
func seedIdentityJournal(ctx context.Context, dbPath string) error {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return err
	}
	journal, err := state.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("seeding a journal at %s: %w", dbPath, err)
	}
	return journal.Close()
}

// writeIdentityConfig writes the smallest configuration `check` accepts,
// naming the journal it is given.
func writeIdentityConfig(path, dbPath, root string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + dbPath + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + filepath.Join(root, "remote") + "\n" +
		"        local_path: " + filepath.Join(root, "local") + "\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	return os.WriteFile(path, []byte(body), 0o644)
}

// sanitizeLabel turns a scenario label into one directory name, so a
// failure names the shape that produced it rather than "scenario-3".
func sanitizeLabel(label string) string {
	out := make([]rune, 0, len(label))
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r == ' ':
			out = append(out, '-')
		}
	}
	return string(out)
}
