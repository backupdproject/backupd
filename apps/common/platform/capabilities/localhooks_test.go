package capabilities_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/retnd/retnd/apps/common/platform/capabilities"
)

// These tests are the executable half of issue #877's acceptance
// criterion: "every provider either supports the Host Workflow Runner +
// container hooks or reports the capability as unavailable through the
// capability contract". The cross-provider matrix in
// distribution/packaging holds the eleven PACKAGED targets to the same
// answers; this file holds the contract itself to being answerable,
// complete, and refused out loud.

// hostAdapter is a PlatformAdapter that reports whichever platform a test
// needs. It embeds BasePlatformAdapter because the local-hook contract
// deliberately adds no method to PlatformAdapter: the answer resolves
// from the adapter's ID, so an adapter written before #877 answers
// correctly without being touched, and cannot answer incorrectly on
// purpose.
type hostAdapter struct {
	capabilities.BasePlatformAdapter
	id capabilities.PlatformID
}

func (h hostAdapter) ID() capabilities.PlatformID { return h.id }

func (h hostAdapter) Capabilities() capabilities.PlatformCapabilities {
	return capabilities.PlatformCapabilities{}
}

func (h hostAdapter) PlatformInfo(_ context.Context) (capabilities.PlatformInfo, error) {
	return capabilities.PlatformInfo{ID: h.id}, nil
}

var _ capabilities.PlatformAdapter = hostAdapter{}

// TestEveryPlatformHasALocalHookAnswer is the completeness guard, and it
// is the one test here that would catch the failure mode that actually
// happens: a platform added to the PlatformID constants and not to the
// resolution table. A map lookup for a missing key returns the zero
// LocalHookSupport, which is "unavailable, no reason, nothing instead" —
// a silent no, which is exactly what this capability exists to prevent.
func TestEveryPlatformHasALocalHookAnswer(t *testing.T) {
	ids := capabilities.AllPlatformIDs()
	if len(ids) == 0 {
		t.Fatal("AllPlatformIDs() is empty, so every table check below would pass vacuously")
	}
	for _, id := range ids {
		support, ok := capabilities.LocalHookSupportFor(id)
		if !ok {
			t.Errorf("platform %q has no declared local-hook answer; add a row to localHookSupport rather than letting the zero value stand in for one", id)
			continue
		}
		if strings.TrimSpace(support.Reason) == "" {
			t.Errorf("platform %q declares Available=%t with no reason; the reason is what an operator reads", id, support.Available)
		}
		if support.Available {
			if support.DockerAccess != capabilities.DockerAccessHostGroup {
				t.Errorf("platform %q says local hooks are available but its DockerAccess is %q: the runner has to reach a daemon somehow, and %q is the only mechanism this contract knows", id, support.DockerAccess, capabilities.DockerAccessHostGroup)
			}
			if support.Instead != "" {
				t.Errorf("platform %q says local hooks are available and still names an alternative (%q); Instead is what an operator gets when the answer is no", id, support.Instead)
			}
			continue
		}
		if support.DockerAccess != capabilities.DockerAccessNone {
			t.Errorf("platform %q says local hooks are unavailable but claims Docker access %q; those two cannot both be true", id, support.DockerAccess)
		}
		if strings.TrimSpace(support.Instead) == "" {
			t.Errorf("platform %q refuses local hooks and names nothing instead; §22 requires an unsupported capability to say what the operator gets in its place", id)
		}
	}
}

// The mirror of the test above. Completeness in one direction only would
// be satisfied by a table that answers for platforms this product does
// not have, which is how a stale row survives a provider's removal.
func TestTheLocalHookTableAnswersForNoPlatformThisContractDoesNotHave(t *testing.T) {
	known := map[capabilities.PlatformID]bool{}
	for _, id := range capabilities.AllPlatformIDs() {
		known[id] = true
	}
	for _, id := range []capabilities.PlatformID{"casaos", "zimaos", "portainer", "dockge", "", "Generic"} {
		if known[id] {
			continue
		}
		if _, ok := capabilities.LocalHookSupportFor(id); ok {
			t.Errorf("the table answers for %q, which is not one of this contract's PlatformIDs; the four container managers and app stores run the generic profile and are answered as `generic`, not as themselves", id)
		}
	}
}

func TestLocalHooksAllowsAHostTheRunnerInstallerOwns(t *testing.T) {
	support, err := capabilities.LocalHooks(hostAdapter{id: capabilities.PlatformGeneric})
	if err != nil {
		t.Fatalf("LocalHooks(generic) = %v, want no error: a generic Docker/Linux host is the runner installer's own target", err)
	}
	if !support.Available {
		t.Error("LocalHooks(generic) reported unavailable")
	}
	if support.DockerAccess != capabilities.DockerAccessHostGroup {
		t.Errorf("LocalHooks(generic).DockerAccess = %q, want %q", support.DockerAccess, capabilities.DockerAccessHostGroup)
	}
}

// The load-bearing test. An appliance platform must refuse, the refusal
// must be recognisable by errors.Is rather than by string matching, and
// it must carry both halves an operator needs: why, and what instead.
func TestLocalHooksRefusesAnApplianceWithAReasonAndAnAlternative(t *testing.T) {
	for _, id := range []capabilities.PlatformID{
		capabilities.PlatformSynology,
		capabilities.PlatformTrueNAS,
		capabilities.PlatformUnraid,
		capabilities.PlatformUGOS,
	} {
		t.Run(string(id), func(t *testing.T) {
			support, err := capabilities.LocalHooks(hostAdapter{id: id})
			if err == nil {
				t.Fatalf("LocalHooks(%s) returned no error; an appliance that cannot host the runner must refuse rather than degrade silently", id)
			}
			if !capabilities.LocalHooksRefused(err) {
				t.Errorf("LocalHooks(%s) error is not recognisable as a local-hook refusal: %v", id, err)
			}
			if !errors.Is(err, capabilities.ErrCapabilityUnsupported) {
				t.Errorf("LocalHooks(%s) error does not satisfy errors.Is(ErrCapabilityUnsupported): %v\n\nA caller that only asks whether the platform is incapable of something must keep working", id, err)
			}
			if support.Available {
				t.Errorf("LocalHooks(%s) refused and still reported Available", id)
			}
			msg := err.Error()
			if !strings.Contains(msg, support.Reason) {
				t.Errorf("LocalHooks(%s) refusal does not carry the reason.\n got: %s\nwant it to contain: %s", id, msg, support.Reason)
			}
			if !strings.Contains(msg, support.Instead) {
				t.Errorf("LocalHooks(%s) refusal does not name what the operator gets instead.\n got: %s\nwant it to contain: %s", id, msg, support.Instead)
			}
		})
	}
}

// An id nobody has decided about is not a "no". Reporting it as one would
// let a new provider ship with local hooks quietly switched off and a
// refusal message that reads as a deliberate decision.
func TestAnUndeclaredPlatformIsUndecidedRatherThanRefused(t *testing.T) {
	_, err := capabilities.LocalHooks(hostAdapter{id: "a-platform-nobody-declared"})
	if err == nil {
		t.Fatal("LocalHooks on an unknown platform returned no error")
	}
	if capabilities.LocalHooksRefused(err) {
		t.Errorf("LocalHooks on an unknown platform reported a local-hook refusal: %v\n\nNobody has decided anything about that platform; presenting undecided as decided is the failure this distinction exists for", err)
	}
	if !errors.Is(err, capabilities.ErrCapabilityUnsupported) {
		t.Errorf("LocalHooks on an unknown platform should still be an unsupported-capability failure, got %v", err)
	}
}

func TestLocalHooksRefusesANilAdapter(t *testing.T) {
	if _, err := capabilities.LocalHooks(nil); !errors.Is(err, capabilities.ErrCapabilityUnsupported) {
		t.Errorf("LocalHooks(nil) = %v, want an unsupported-capability failure rather than a panic", err)
	}
}

// PreflightLocalHooks is the shape a preflight step consumes, and the
// thing worth pinning is that it agrees with LocalHooks on every
// platform: two entry points that could disagree would mean a deployment
// whose preflight passes and whose runtime refuses.
func TestPreflightAgreesWithTheContractOnEveryPlatform(t *testing.T) {
	for _, id := range capabilities.AllPlatformIDs() {
		adapter := hostAdapter{id: id}
		_, want := capabilities.LocalHooks(adapter)
		got := capabilities.PreflightLocalHooks(context.Background(), adapter)
		if (got == nil) != (want == nil) {
			t.Errorf("platform %q: PreflightLocalHooks = %v but LocalHooks = %v", id, got, want)
		}
	}
}

// The five providers whose answer is no are a deliberate, reviewed set,
// and pinning it is what keeps the tests above from being vacuous: if
// every platform said yes, the refusal tests would simply not run, and if
// every platform said no, nothing would notice that the product lost a
// feature. The pin is per platform rather than a count, so flipping one
// answer names the platform in the failure.
func TestTheSupportedAndRefusedPlatformsAreTheReviewedSet(t *testing.T) {
	want := map[capabilities.PlatformID]bool{
		capabilities.PlatformGeneric:        true,
		capabilities.PlatformOpenMediaVault: true,
		capabilities.PlatformProxmox:        true,
		capabilities.PlatformTrueNAS:        false,
		capabilities.PlatformUnraid:         false,
		capabilities.PlatformSynology:       false,
		capabilities.PlatformUGOS:           false,
	}
	for _, id := range capabilities.AllPlatformIDs() {
		support, ok := capabilities.LocalHookSupportFor(id)
		if !ok {
			continue // TestEveryPlatformHasALocalHookAnswer reports this.
		}
		expected, declared := want[id]
		if !declared {
			t.Errorf("platform %q has a local-hook answer that this pin does not cover; decide it here too, with a reason, rather than letting a new platform inherit whichever answer its row happened to get", id)
			continue
		}
		if support.Available != expected {
			t.Errorf("platform %q local hooks available = %t, want %t.\n\nIf this is a deliberate change, the conformance matrix cell in distribution/packaging/conformance.json and the platform's acceptance procedure move with it — an operator reads those, not this table.", id, support.Available, expected)
		}
	}
}
