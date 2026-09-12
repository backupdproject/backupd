package secretref_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/secretref"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// theSecret is the material every test in this file resolves. It is
// deliberately one distinctive token so a leak assertion can look for it in
// a rendered error and find it if it is there.
const theSecret = "canary-repository-passphrase-8f2a"

func writeFile(t *testing.T, name, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}

	return path
}

// TestRefFieldSetMatchesTheConfiguredOnes is the anti-drift pin, and it is
// the whole argument that this package is not a second credential
// vocabulary.
//
// An operator declares a secret in exactly one shape in config.yaml
// (file/env/command). config.MediumCredentials, config.Passphrase and
// transport.MediumCredentials all say it; this package resolves it. A
// fourth field appearing on one of them and not the others would be a
// source an operator can write and this resolver silently ignores, which
// is the failure mode that makes a "no parallel secret store" claim stop
// being true.
func TestRefFieldSetMatchesTheConfiguredOnes(t *testing.T) {
	t.Parallel()

	want := fieldShape(reflect.TypeFor[secretref.Ref]())

	for _, other := range []struct {
		name string
		typ  reflect.Type
	}{
		{"transport.MediumCredentials", reflect.TypeFor[transport.MediumCredentials]()},
		{"config.MediumCredentials", reflect.TypeFor[config.MediumCredentials]()},
		{"config.Passphrase", reflect.TypeFor[config.Passphrase]()},
	} {
		if got := fieldShape(other.typ); got != want {
			t.Errorf("%s has fields %s; secretref.Ref has %s. These are the same declaration in two places, "+
				"so a field on one and not the other is a credential source an operator can write and this package cannot resolve",
				other.name, got, want)
		}
	}
}

// fieldShape renders a struct's exported field names and kinds, which is
// what must match: the yaml tags legitimately differ between the schema
// types and this one, and the doc comments certainly do.
func fieldShape(t reflect.Type) string {
	var parts []string

	for i := range t.NumField() {
		f := t.Field(i)
		parts = append(parts, f.Name+" "+f.Type.String())
	}

	return "{" + strings.Join(parts, "; ") + "}"
}

// TestRefValidateDemandsExactlyOneSource covers the two ways a reference
// can be unusable, and they are different operator mistakes: silence means
// nothing was declared, and two sources mean two things were and this
// package will not choose between them.
func TestRefValidateDemandsExactlyOneSource(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ref  secretref.Ref
		want error
	}{
		{"none", secretref.Ref{}, secretref.ErrNoSource},
		{"file and env", secretref.Ref{File: "/tmp/x", Env: "Y"}, secretref.ErrAmbiguousSource},
		{"file and command", secretref.Ref{File: "/tmp/x", Command: []string{"/bin/true"}}, secretref.ErrAmbiguousSource},
		{"env and command", secretref.Ref{Env: "Y", Command: []string{"/bin/true"}}, secretref.ErrAmbiguousSource},
		{"all three", secretref.Ref{File: "/tmp/x", Env: "Y", Command: []string{"/bin/true"}}, secretref.ErrAmbiguousSource},
		{"file", secretref.Ref{File: "/tmp/x"}, nil},
		{"env", secretref.Ref{Env: "Y"}, nil},
		{"command", secretref.Ref{Command: []string{"/bin/true"}}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if err := tc.ref.Validate(); !errors.Is(err, tc.want) {
				t.Errorf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestResolveFromEveryDeclaredSource is the round trip: whichever of the
// three an operator wrote, the same material comes back.
func TestResolveFromEveryDeclaredSource(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("file", func(t *testing.T) {
		t.Parallel()

		// The trailing newline is what every editor and every `echo >`
		// leaves behind, and a passphrase with an invisible newline on the
		// end is a repository nobody can open.
		ref := secretref.Ref{File: writeFile(t, "passphrase", theSecret+"\n")}

		got, err := secretref.Resolve(ctx, ref)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}

		if got.Reveal() != theSecret {
			t.Errorf("resolved %d bytes, want the %d-byte secret with its trailing newline removed", len(got.Reveal()), len(theSecret))
		}
	})

	t.Run("command", func(t *testing.T) {
		t.Parallel()

		got, err := secretref.Resolve(ctx, secretref.Ref{Command: []string{"/bin/echo", theSecret}})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}

		if got.Reveal() != theSecret {
			t.Errorf("resolved the wrong material from the command")
		}
	})
}

// TestResolveFromTheEnvironment is separate and not parallel because
// t.Setenv cannot be used from a parallel test: the environment is
// process-wide state and the testing package refuses to let two tests
// disagree about it.
func TestResolveFromTheEnvironment(t *testing.T) {
	t.Setenv("BACKUPD_TEST_SECRET", theSecret+"\n")

	got, err := secretref.Resolve(context.Background(), secretref.Ref{Env: "BACKUPD_TEST_SECRET"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if got.Reveal() != theSecret {
		t.Errorf("resolved the wrong material from the environment")
	}
}

// TestResolveRefusesAnEmptyAnswer is the refusal that keeps a broken
// resolver from looking like a correctly-resolved empty passphrase, which
// downstream is an unopenable repository reported as a wrong password.
func TestResolveRefusesAnEmptyAnswer(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	for _, tc := range []struct {
		name string
		ref  secretref.Ref
	}{
		{"empty file", secretref.Ref{File: writeFile(t, "empty", "")}},
		{"whitespace-only file", secretref.Ref{File: writeFile(t, "blank", "  \n\t\n")}},
		{"command that prints nothing", secretref.Ref{Command: []string{"/usr/bin/true"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := secretref.Resolve(ctx, tc.ref); !errors.Is(err, secretref.ErrEmpty) {
				t.Errorf("Resolve() = %v, want ErrEmpty", err)
			}
		})
	}
}

// TestResolveNeverEchoesTheMaterial is the leak assertion, run over every
// error path a resolver can take with real material in hand.
//
// The dangerous one is the file source: the shell-credentials text and the
// path are both in scope at the moment the refusal is built, and a %q of
// the wrong variable is a one-character review miss.
func TestResolveNeverEchoesTheMaterial(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// A file whose contents are fine but which is far too large, so the
	// refusal happens with the material available.
	huge := writeFile(t, "huge", theSecret+strings.Repeat("x", 128<<10))

	// A command that prints the secret and then fails, so both the exit
	// status and the material are in scope.
	failing := secretref.Ref{Command: []string{"/bin/sh", "-c", "echo " + theSecret + "; exit 3"}}

	for _, tc := range []struct {
		name string
		ref  secretref.Ref
	}{
		{"oversized file", secretref.Ref{File: huge}},
		{"failing command", failing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			secret, err := secretref.Resolve(ctx, tc.ref)
			if err == nil {
				t.Fatalf("Resolve did not refuse")
			}

			assertNoSecret(t, "error", err.Error())
			assertNoSecret(t, "%v of the secret returned beside the error", fmt.Sprintf("%v", secret))
		})
	}
}

// TestRefStringNamesTheSourceNeverTheMaterial matters because a Ref is
// exactly the thing that IS safe to log, and the whole custody story
// depends on it staying that way: a Ref carrying a resolved value would
// have every location that logs a repository location logging a passphrase.
func TestRefStringNamesTheSourceNeverTheMaterial(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "passphrase", theSecret)

	for _, tc := range []struct {
		name string
		ref  secretref.Ref
		want string
	}{
		{"file", secretref.Ref{File: path}, "file " + path},
		{"env", secretref.Ref{Env: "SOME_VAR"}, "env SOME_VAR"},
		{"command", secretref.Ref{Command: []string{"/bin/vault", "read", "secret"}}, "command /bin/vault"},
		{"unset", secretref.Ref{}, "no source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.ref.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRefNeverRendersTheResolverArgvOnAnyPath is the CRITICAL half of the
// "a Ref is safe to log" claim, and String() is not enough to make it.
//
// Production logging goes through slog.NewJSONHandler, which does not
// consult fmt.Stringer: it reflects over the value and emits every
// exported field, so a Ref logged as a structured attribute used to print
// its whole Command array -- the vault path, the role and, in the shape
// below, a token. encoding/json does the same to any struct a Ref is a
// field of, and %#v does it to a debug print. Each rendering below is a
// separate mechanism with its own opt-in method, which is why each one is
// asserted rather than assumed to follow from String().
func TestRefNeverRendersTheResolverArgvOnAnyPath(t *testing.T) {
	t.Parallel()

	ref := secretref.Ref{Command: []string{"/bin/vault", "read", "--token", argvCanary}}

	encoded, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("json.Marshal(Ref): %v", err)
	}

	// A Ref is a field of a repository location, and that is how it
	// reaches a diagnostic dump or an API response: inside something else.
	type location struct {
		Domain     string
		Passphrase secretref.Ref
	}

	nested, err := json.Marshal(location{Domain: "production", Passphrase: ref})
	if err != nil {
		t.Fatalf("json.Marshal(struct holding a Ref): %v", err)
	}

	text, err := ref.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}

	var logged, loggedNested strings.Builder

	slog.New(slog.NewJSONHandler(&logged, nil)).Info("opening repository", "passphrase", ref)
	slog.New(slog.NewJSONHandler(&loggedNested, nil)).Info("opening repository",
		"location", location{Domain: "production", Passphrase: ref})

	for _, rendering := range []struct {
		label string
		text  string
	}{
		{"%v", fmt.Sprintf("%v", ref)},
		{"%+v", fmt.Sprintf("%+v", ref)},
		{"%#v", fmt.Sprintf("%#v", ref)},
		{"json", string(encoded)},
		{"json inside a struct", string(nested)},
		{"MarshalText", string(text)},
		{"slog json handler", logged.String()},
		{"slog json handler, nested in a struct", loggedNested.String()},
	} {
		if strings.Contains(rendering.text, argvCanary) {
			t.Errorf("the %s rendering carries the resolver's token: %s", rendering.label, rendering.text)
		}

		// The whole argv is refused, not only the token in it: the
		// argument that carries a secret is whichever one the operator's
		// secrets manager takes it in, and this package does not get to
		// guess which position that is.
		if strings.Contains(rendering.text, "--token") || strings.Contains(rendering.text, "read") {
			t.Errorf("the %s rendering carries the resolver's arguments: %s", rendering.label, rendering.text)
		}

		// And the executable IS rendered, on every path. A redaction that
		// hid which resolver ran would make a broken resolver
		// undiagnosable, which is the mistake in the other direction.
		if !strings.Contains(rendering.text, "/bin/vault") {
			t.Errorf("the %s rendering does not name the resolver executable, so a broken resolver cannot be found: %s",
				rendering.label, rendering.text)
		}
	}
}

// argvCanary is the token the resolver invocation above carries in its
// arguments, which is where a secrets-manager call really does put one.
const argvCanary = "s.canary-vault-token-6d41b0"

// TestResolveRefusesASecretFileOutOfCustody is the file source's half of
// the custody model, and it is the same rule the medium plane already
// enforces on a credentials file (internal/transport/rclone/mediumcreds.go):
// a secret this process reads must be one no other local account could
// read, replace or substitute.
//
// Every row is a real deployment mistake rather than an exotic one, and
// each one defeats the custody argument completely on its own: a
// world-readable passphrase is readable by every account on the NAS, a
// group-writable containing directory lets any member of that group swap
// the file whatever its own mode says, and a fifo or a device in place of
// the file is material supplied by whoever opened the other end.
func TestResolveRefusesASecretFileOutOfCustody(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	for _, tc := range []struct {
		name string
		// path returns the file to declare, having put whatever the case
		// is about in place.
		path func(t *testing.T) string
	}{
		{"world-readable", func(t *testing.T) string {
			t.Helper()

			return writeFileMode(t, "world-readable", theSecret, 0o644)
		}},
		{"group-readable", func(t *testing.T) string {
			t.Helper()

			return writeFileMode(t, "group-readable", theSecret, 0o640)
		}},
		{"a symbolic link", func(t *testing.T) string {
			t.Helper()

			real := writeFile(t, "passphrase", theSecret)
			link := filepath.Join(t.TempDir(), "link-to-passphrase")

			if err := os.Symlink(real, link); err != nil {
				t.Fatalf("creating a symbolic link: %v", err)
			}

			return link
		}},
		{"a fifo rather than a file", func(t *testing.T) string {
			t.Helper()

			path := filepath.Join(t.TempDir(), "fifo")
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatalf("creating a fifo: %v", err)
			}

			return path
		}},
		{"a directory rather than a file", func(t *testing.T) string {
			t.Helper()

			return t.TempDir()
		}},
		{"a group-writable containing directory", func(t *testing.T) string {
			t.Helper()

			dir := filepath.Join(t.TempDir(), "shared")
			if err := os.Mkdir(dir, 0o770); err != nil {
				t.Fatalf("creating a group-writable directory: %v", err)
			}

			// Past the umask, which would otherwise take the group write
			// bit straight back off and make this row assert nothing.
			if err := os.Chmod(dir, 0o770); err != nil {
				t.Fatalf("chmod 0770: %v", err)
			}

			path := filepath.Join(dir, "passphrase")
			if err := os.WriteFile(path, []byte(theSecret), 0o600); err != nil {
				t.Fatalf("writing the secret file: %v", err)
			}

			return path
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := tc.path(t)

			secret, err := secretref.Resolve(ctx, secretref.Ref{File: path})
			if !errors.Is(err, secretref.ErrCustody) {
				t.Fatalf("Resolve(%s) = %v, want ErrCustody", tc.name, err)
			}

			// The refusal names the path so an operator can fix it, and
			// carries nothing that was behind it.
			if !strings.Contains(err.Error(), path) {
				t.Errorf("the refusal does not name the file it refused: %v", err)
			}

			assertNoSecret(t, "the custody refusal", err.Error())
			assertNoSecret(t, "the secret returned beside it", fmt.Sprintf("%v", secret))
		})
	}
}

// TestResolveAcceptsATightlyHeldSecretFile is the other side of the rows
// above, and it is what makes them a rule rather than a refusal of
// everything: the mode this project writes a secret file with is accepted,
// and so is the read-only 0400 an operator hand-writes.
func TestResolveAcceptsATightlyHeldSecretFile(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	for _, mode := range []os.FileMode{0o600, 0o400, 0o700} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			t.Parallel()

			secret, err := secretref.Resolve(ctx, secretref.Ref{File: writeFileMode(t, "passphrase", theSecret, mode)})
			if err != nil {
				t.Fatalf("Resolve of a %04o secret file: %v", mode, err)
			}

			if secret.Reveal() != theSecret {
				t.Errorf("Resolve returned material that is not what the file held")
			}
		})
	}
}

// writeFileMode writes a secret file with an explicit mode, for the
// custody rows. It bypasses os.WriteFile's umask interaction by chmoding
// after the write, so a permissive row really is permissive on every
// machine.
func writeFileMode(t *testing.T, name, contents string, mode os.FileMode) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}

	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %04o %s: %v", mode, name, err)
	}

	return path
}

// assertNoSecret fails when text contains the material this package is
// entrusted with, or any prefix of it long enough to be useful.
func assertNoSecret(t *testing.T, label, text string) {
	t.Helper()

	if strings.Contains(text, theSecret) {
		t.Errorf("%s contains the resolved secret: %s", label, text)
	}

	// "in whole or in part": a prefix long enough to narrow a brute force
	// is a leak too, and truncation is exactly how one gets there.
	if prefix := theSecret[:12]; strings.Contains(text, prefix) {
		t.Errorf("%s contains a %d-character prefix of the resolved secret: %s", label, len(prefix), text)
	}
}
