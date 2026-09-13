package capabilities

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// This file is the platform half of EPIC L's local workflow hooks
// (issue #877). Two merged features made a Docker daemon a hard
// prerequisite for one product feature, and neither of them is a fact
// about the engine:
//
//   - L2 (#809) provisions a Host Workflow Runner beside the distroless
//     engine: a small version-pinned systemd unit that runs as its own
//     unprivileged account;
//   - #865 made every LOCAL hook run in an ephemeral Docker container, so
//     that runner now needs a reachable daemon, membership of the
//     socket's group, and a pullable hook image
//     (docs/adr/0020-host-workflow-runner.md Decision 9,
//     docs/runtime-contract.md).
//
// Those three prerequisites are host facts, and the eleven platforms this
// product supports differ on all of them: some are ordinary Linux hosts
// the operator administers, and some are appliances whose management
// plane §4A/§75 forbids this product to modify at all. So the answer
// belongs here, in the one contract every provider composes over, rather
// than in one provider's installer branch.
//
// # Why this is not a PlatformCapabilities field
//
// PlatformCapabilities is the BROWSER-HOST capability model: it mirrors
// ui/shared's PlatformCapabilities field-for-field and is what GET
// /api/v1/system/capabilities reports, which is why
// profile.UndeliverableCapabilities can say of three of its five fields
// that "a capabilities.PlatformAdapter has no collaborator that could
// deliver it". Local-hook readiness is the opposite kind of fact: it is
// decided entirely on the host, by a unit that is not this process, and
// putting it in that struct would either reshape the UI's wire contract
// or leave a field nothing reports. It gets its own accessor for the same
// reason Authenticator and Notifier are accessors rather than strings.
//
// # Why the answer is a table rather than an adapter method
//
// A provider cannot be trusted to answer this about itself, and it does
// not need to: the question is decided by the platform's SHAPE, and
// PlatformID is already a closed set. A per-adapter method would be an
// override hole — a provider could return "available" on an appliance
// that cannot run a systemd unit, which is precisely the silent
// degradation #877 exists to prevent. So LocalHooks resolves from the
// adapter's own ID against localHookSupport below, exactly the way
// apps/common/platform/notify resolves an alert sink from
// Capabilities().NativeNotifications rather than trusting a sink to
// self-declare.
//
// # What this does NOT decide
//
// Whether a daemon is reachable RIGHT NOW. That is a live fact about one
// host, and two things already answer it and already refuse loudly:
// scripts/install/install_docker_host.py's preflight (which names the
// `usermod -aG` line when the service account cannot reach the socket)
// and the runner's own startup capability probe, which refuses to serve
// local hooks at all rather than falling back to the host's shell. This
// contract answers the question those two cannot: whether it is even
// worth asking on this platform, and what an operator gets instead when
// it is not.

// DockerAccess names how a platform gives the Host Workflow Runner's
// service account a reachable Docker daemon. There are exactly two
// answers because there are exactly two shapes: a host the operator
// administers, and an appliance.
type DockerAccess string

const (
	// DockerAccessNone: this platform offers no supported way to give an
	// account access to a Docker daemon. Local hooks cannot run here.
	DockerAccessNone DockerAccess = "none"
	// DockerAccessHostGroup: the operator administers the host, so
	// membership of the Docker socket's group can be granted to the
	// runner's account (`usermod -aG <group> <account>`) and a systemd
	// unit can be installed. This is what
	// scripts/install/install_docker_host.py provisions.
	DockerAccessHostGroup DockerAccess = "host-docker-group"
)

// LocalHookSupport is one platform's answer about local workflow hooks.
//
// Every field is operator-facing on purpose. A bare boolean would satisfy
// every caller and tell nobody anything: §22's rule is that an
// unsupported capability is explicit and names what happens INSTEAD, so
// the refusal carries the reason it is refused and the mechanism that
// still works.
type LocalHookSupport struct {
	// Available reports whether a Host Workflow Runner can be
	// provisioned on this platform at all, which is the precondition for
	// any local hook running. It is not a promise that a daemon is
	// reachable on a particular host; see the package comment above.
	Available bool
	// DockerAccess is how the runner's account gets to the daemon here.
	DockerAccess DockerAccess
	// Reason is why this platform answers the way it does, in a sentence
	// an operator can act on. Always set, for both answers: "yes,
	// because the installer owns this host" is as load-bearing as the no.
	Reason string
	// Instead names the mechanism an operator gets when Available is
	// false, and is empty when it is true. Never empty for an
	// unavailable platform: a "no" with no alternative beside it is the
	// half-answer §22 forbids.
	Instead string
}

// ErrLocalHooksUnavailable is the refusal LocalHooks returns for a
// platform that cannot run the Host Workflow Runner.
//
// It wraps ErrCapabilityUnsupported rather than standing beside it, so a
// caller that only asks "is this platform incapable of that" keeps
// working unchanged, and a caller that specifically handles local hooks
// can still tell this refusal from a missing notifier.
var ErrLocalHooksUnavailable = fmt.Errorf("local workflow hooks cannot run on this platform: %w", ErrCapabilityUnsupported)

// localHookSupport is the per-platform resolution: one row per
// PlatformID, no defaults and no fallthrough.
//
// The discriminator is one question, asked the same way of every row:
// can this product's own installer provision a systemd unit here and can
// the operator grant that unit's account membership of the Docker
// socket's group, WITHOUT modifying a vendor management plane? Where the
// deployment sits on a Linux host the operator administers the answer is
// yes; where the platform is an appliance the answer is no, and it is no
// for the same reason every provider package in this repository already
// installs no host unit and writes no vendor config database
// (§4A/§75, the host-management-plane-untouched conformance capability).
//
// A missing row is a compile-time-invisible failure, so
// TestEveryPlatformHasALocalHookAnswer holds this map to the PlatformID
// constants in both directions.
var localHookSupport = map[PlatformID]LocalHookSupport{
	PlatformGeneric: {
		Available:    true,
		DockerAccess: DockerAccessHostGroup,
		Reason: "A generic Docker/Linux host is the runner installer's own target: " +
			"scripts/install/install_docker_host.py provisions backupd-workflow-runner.service, " +
			"puts the service account in the Docker socket's group, and fetches the pinned hook image.",
	},
	PlatformOpenMediaVault: {
		Available:    true,
		DockerAccess: DockerAccessHostGroup,
		Reason: "OpenMediaVault is Debian with systemd and the Docker Compose plugin, and the deployment " +
			"is an ordinary compose project on that host, so the same host installer provisions the " +
			"runner beside it. OMV's own configuration database is never touched.",
	},
	PlatformProxmox: {
		Available:    true,
		DockerAccess: DockerAccessHostGroup,
		Reason: "The one supported Proxmox VE model is a dedicated container-host guest the operator " +
			"administers (docs/acceptance/proxmox-ve-deployment.md), which is an ordinary Linux host " +
			"with systemd and Docker. The runner is provisioned inside that guest and never on the PVE host.",
	},
	PlatformTrueNAS: {
		Available:    false,
		DockerAccess: DockerAccessNone,
		Reason: "TrueNAS's host is vendor-managed: applications run under the middleware's own container " +
			"runtime, a third-party systemd unit is unsupported and does not survive an upgrade, and " +
			"there is no supported way to add an account to the Docker socket's group. Doing it anyway " +
			"is the host-management-plane modification §4A/§75 forbids this product.",
		Instead: "Run the hook remotely: a workflow step with a remote target runs over SSH " +
			"(docs/adr/0021-remote-ssh-exec.md) and needs no Docker on the NAS.",
	},
	PlatformUnraid: {
		Available:    false,
		DockerAccess: DockerAccessNone,
		Reason: "Unraid rebuilds its operating system from the flash device on every boot, so there is no " +
			"persistent host unit for the runner to be, and its Docker runs everything as root while " +
			"the runner refuses to run as root at all.",
		Instead: "Run the hook remotely: a workflow step with a remote target runs over SSH " +
			"(docs/adr/0021-remote-ssh-exec.md) and needs no Docker on the NAS.",
	},
	PlatformSynology: {
		Available:    false,
		DockerAccess: DockerAccessNone,
		Reason: "A DSM package cannot install a systemd unit or grant a supplementary group, and Container " +
			"Manager's socket is root-owned; either step would be the host-management-plane " +
			"modification §4A/§75 forbids this product.",
		Instead: "Run the hook remotely: a workflow step with a remote target runs over SSH " +
			"(docs/adr/0021-remote-ssh-exec.md) and needs no Docker on the NAS.",
	},
	PlatformUGOS: {
		Available:    false,
		DockerAccess: DockerAccessNone,
		Reason: "UGOS Pro is a closed appliance: it offers no supported way to install a host unit or to " +
			"add an account to the Docker socket's group.",
		Instead: "Run the hook remotely: a workflow step with a remote target runs over SSH " +
			"(docs/adr/0021-remote-ssh-exec.md) and needs no Docker on the NAS.",
	},
}

// AllPlatformIDs returns every provider identifier this contract
// recognises, sorted.
//
// It exists so that a table keyed by PlatformID can be held to the
// constant set rather than to a hand-copied list of it: a provider added
// to the consts above and forgotten here would otherwise acquire whatever
// a map lookup returns for a missing key, which for LocalHookSupport is
// the zero value — "unavailable, with no reason and nothing instead", the
// one answer this contract must never give silently.
func AllPlatformIDs() []PlatformID {
	ids := []PlatformID{
		PlatformGeneric,
		PlatformUGOS,
		PlatformSynology,
		PlatformTrueNAS,
		PlatformUnraid,
		PlatformOpenMediaVault,
		PlatformProxmox,
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// LocalHookSupportFor returns one platform's local-hook answer. The
// second result is false for an identifier this contract does not
// recognise, which is a different thing from a platform that answers no:
// an unknown id means nobody has decided, and a caller must not present
// that as a decision.
func LocalHookSupportFor(id PlatformID) (LocalHookSupport, bool) {
	s, ok := localHookSupport[id]
	return s, ok
}

// LocalHooks answers whether adapter's platform can run local workflow
// hooks, and refuses with a reason when it cannot.
//
// The error is the interesting half. A caller wiring a deployment up
// should ask once, at startup or at preflight, and report the refusal
// verbatim: the whole point of #877 is that an operator on a platform
// that cannot run the runner is TOLD so, with the alternative named,
// rather than finding out when a backup's hook is refused at run time.
//
// It takes the adapter rather than a bare PlatformID so that the answer
// is always about the platform the running process actually believes it
// is on, and so a caller cannot accidentally ask about a different one
// than the one it wired.
func LocalHooks(adapter PlatformAdapter) (LocalHookSupport, error) {
	if adapter == nil {
		return LocalHookSupport{}, fmt.Errorf("capabilities: a platform adapter is required: %w", ErrCapabilityUnsupported)
	}
	id := adapter.ID()
	support, ok := LocalHookSupportFor(id)
	if !ok {
		// Not a refusal about local hooks: nobody has decided anything
		// about this platform, and saying "unavailable" would be
		// inventing a decision. Deliberately NOT wrapping
		// ErrLocalHooksUnavailable.
		return LocalHookSupport{}, fmt.Errorf("capabilities: no local-hook answer is declared for platform %q: %w", id, ErrCapabilityUnsupported)
	}
	if !support.Available {
		return support, fmt.Errorf("capabilities: %s: %s %s: %w", id, support.Reason, support.Instead, ErrLocalHooksUnavailable)
	}
	return support, nil
}

// LocalHooksRefused reports whether err is a platform's refusal to run
// local workflow hooks, as opposed to any other capability failure.
// Provided so a caller does not have to know that the refusal wraps two
// errors.
func LocalHooksRefused(err error) bool { return errors.Is(err, ErrLocalHooksUnavailable) }

// PreflightLocalHooks is LocalHooks for a caller that only needs the
// verdict, in the shape a preflight step consumes: nil means go ahead.
// The context is accepted and unused for the same reason
// PlatformInfo's is — the platform-shape answer needs nothing asked of
// the host — and keeping it means the live daemon probe a future runner
// client supplies can take this call's place without changing its
// callers.
func PreflightLocalHooks(_ context.Context, adapter PlatformAdapter) error {
	_, err := LocalHooks(adapter)
	return err
}
