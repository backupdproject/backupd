package packaging

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// This file is the packaging half of issue #877: the cross-provider
// conformance matrix's `local-workflow-hooks` capability, and the readers
// that let it be decided from checked-in files rather than asserted.
//
// The feature it is about is not a container the store installs. L2
// (#809) provisions a Host Workflow Runner beside the distroless engine,
// and #865 made every local hook run in an ephemeral Docker container, so
// the runner needs a reachable daemon, membership of the socket's group
// and a pinned hook image. None of that reaches the engine container —
// docs/runtime-contract.md is explicit that "this container gains
// nothing" — which is exactly why it needs a conformance row of its own:
// a prerequisite that changes nothing in the shipped compose file is a
// prerequisite no existing packaging check can see.
//
// Three declarations have to agree for a provider, and the check below
// is the thing that makes disagreement fail:
//
//  1. the capability contract, apps/common/platform/capabilities, whose
//     localHookSupport table answers per runtime platform;
//  2. conformance.json's own per-provider workflowRunner block, which is
//     what the matrix reports;
//  3. the document an operator actually reads for that provider.
//
// Two of those three could be made to agree by editing one file, which
// is why the third is a document: the failure #877 exists to prevent is
// an operator on an appliance discovering at backup time that local hooks
// never ran, and only the document can prevent that.

// The two answers a platform can give about local workflow hooks. They
// are the packaging-side spelling of
// capabilities.LocalHookSupport.Available, kept as strings because
// conformance.json is read by people as often as by this package and
// "available" survives a reviewer's eye better than true.
const (
	LocalHooksAvailable   = "available"
	LocalHooksUnavailable = "unavailable"
)

// WorkflowRunner is one provider's own statement about the Host Workflow
// Runner, in conformance.json.
//
// It sits beside Metadata and ArchitectureClaim for the same reason those
// do: a repository-wide fact must not be able to stand in for eleven
// per-provider answers. The runner is provisioned once per HOST, and the
// eleven providers deploy onto hosts that differ on whether a systemd
// unit and a supplementary group can be granted at all.
type WorkflowRunner struct {
	// LocalHooks is "available" or "unavailable": can a Host Workflow
	// Runner be provisioned for this provider's deployment, so that
	// local hooks can run in containers?
	LocalHooks string `json:"localHooks"`
	// Platform is the capabilities.PlatformID whose row in the
	// capability contract decides this provider's runtime answer. Empty
	// for a provider that ships no runtime profile of its own — the four
	// container managers and app stores, which select the generic
	// profile (canonical.json's uiBridge "none" comment) and therefore
	// inherit generic's answer rather than having one.
	Platform string `json:"platform"`
	// Doc is the operator-visible document that must state this
	// provider's answer: the runner install, the Docker prerequisite and
	// whether local hooks are available. Relative to the repository
	// root.
	Doc string `json:"doc"`
}

// Where the capability contract lives. Read textually, the same way
// BridgeDeclaresCapability reads a provider bridge: this package must not
// import apps/ (it is distribution, below the application layer, and
// distribution/ is its own module), and a textual read is also what makes
// a DELETED table row a failure here rather than a compile error nobody
// in this package would see.
const (
	LocalHookContractFile = "apps/common/platform/capabilities/localhooks.go"
	PlatformIDConstFile   = "apps/common/platform/capabilities/capabilities.go"
)

var (
	// platformIDConstRe pulls `PlatformGeneric PlatformID = "generic"`
	// out of the capability contract's closed identifier set.
	platformIDConstRe = regexp.MustCompile(`(?m)^\s*(Platform[A-Za-z]+)\s+PlatformID\s*=\s*"([a-z0-9-]+)"`)
	// localHookTableRe isolates the resolution table, so a struct
	// literal elsewhere in the file cannot be mistaken for a row.
	localHookTableRe = regexp.MustCompile(`(?s)var localHookSupport = map\[PlatformID\]LocalHookSupport\{(.*?)\n\}`)
	// localHookRowRe pulls one row's platform constant and its
	// availability. Availability is read from the field rather than
	// inferred from anything else in the row: it is the one value the
	// whole capability turns on.
	localHookRowRe = regexp.MustCompile(`(Platform[A-Za-z]+):\s*\{[^{}]*?Available:\s*(true|false)`)
)

// ReadLocalHookContract returns platform id -> "may local hooks run
// there", as apps/common/platform/capabilities declares it.
//
// It fails rather than returning a partial answer: every row the table
// holds must resolve to a declared PlatformID, because a row naming a
// constant that no longer exists is a row that answers for nothing while
// still looking like an answer.
func ReadLocalHookContract() (map[string]string, error) {
	return readLocalHookContractFrom(Path(LocalHookContractFile), Path(PlatformIDConstFile))
}

// readLocalHookContractFrom is the body, taking both files as arguments
// for the reason releaseManifestIntegrity takes its manifest: a reader
// whose refusals can only be observed by breaking the real repository is
// a reader whose refusals are never observed at all. Every failure below
// is a way the contract can move out from under this package, and each
// one has to be a refusal rather than an empty map — an empty contract
// would make every cross-layer comparison vacuous and fail every cell
// for the wrong reason.
func readLocalHookContractFrom(contractPath, constPath string) (map[string]string, error) {
	consts, err := readPlatformIDConsts(constPath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(contractPath)
	if err != nil {
		return nil, fmt.Errorf("packaging: read the local-hook capability contract: %w", err)
	}
	table := localHookTableRe.FindStringSubmatch(string(data))
	if table == nil {
		return nil, fmt.Errorf("packaging: %s no longer declares `var localHookSupport = map[PlatformID]LocalHookSupport{...}`; the cross-provider matrix reads that table to decide the local-workflow-hooks capability, so move the check with it rather than deleting it", LocalHookContractFile)
	}
	rows := localHookRowRe.FindAllStringSubmatch(table[1], -1)
	if len(rows) == 0 {
		return nil, fmt.Errorf("packaging: the localHookSupport table in %s declares no rows", LocalHookContractFile)
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		id, ok := consts[row[1]]
		if !ok {
			return nil, fmt.Errorf("packaging: the localHookSupport table names %s, which is not a declared PlatformID in %s", row[1], PlatformIDConstFile)
		}
		answer := LocalHooksUnavailable
		if row[2] == "true" {
			answer = LocalHooksAvailable
		}
		out[id] = answer
	}
	return out, nil
}

// readPlatformIDConsts maps the contract's Go constant names to the wire
// identifiers they hold.
func readPlatformIDConsts(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("packaging: read the PlatformID constants: %w", err)
	}
	matches := platformIDConstRe.FindAllStringSubmatch(string(data), -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("packaging: %s declares no `Platform<Name> PlatformID = \"<id>\"` constants", PlatformIDConstFile)
	}
	out := make(map[string]string, len(matches))
	for _, m := range matches {
		out[m[1]] = m[2]
	}
	return out, nil
}

// The statements a provider's document has to make. Regexes rather than
// substrings because what matters is that the operator is told a fact,
// not that a particular sentence was copied: "add the account to the
// docker group" and "`sudo usermod -aG docker backupd`" are the same
// instruction, and a check that only accepted one of them would be
// satisfied by rewording rather than by documenting.
var (
	// runnerUnitRe: the thing that gets installed. Both spellings for
	// exactly one release: #890 renamed the unit to
	// retnd-workflow-runner.service (the installer's
	// WORKFLOW_RUNNER_UNIT moved with it, and it keeps the old name as a
	// lookup-and-remove spelling), and #891 reworded the provider
	// procedures that name it, so every one of them now says the new
	// name. The alternative stays because a procedure written for a host
	// installed before #890 is still telling its operator a true thing
	// about that host; #895 drops it.
	runnerUnitRe = regexp.MustCompile(`(retnd|backupd)-workflow-runner\.service`)
	// dockerGroupRe: the grant the runner's account needs. #865's whole
	// cost is this one membership, and the installer's refusal names the
	// same command.
	dockerGroupRe = regexp.MustCompile(`(?i)usermod -aG|docker group`)
	// hookImageRe: the image a local hook runs in has to be present
	// before a hook can run, because the runner refuses to pull one.
	hookImageRe = regexp.MustCompile(`WORKFLOW_RUNNER_HOOK_IMAGE|--hook-image|hook image`)
	// hooksUnavailableRe: the sentence an appliance's document must
	// carry, and the sentence a supporting platform's document must NOT.
	hooksUnavailableRe = regexp.MustCompile(`(?i)local (workflow )?hooks (are )?(not available|unavailable)`)
	// refusalMechanismRe: the fact that makes an unavailable answer safe
	// rather than silent, and it has to be attributed.
	//
	// It used to be `refus|preflight`, which the review caught: five
	// procedures satisfied it by saying the INSTALLER'S PREFLIGHT
	// refuses to provision a runner on that platform, and no such
	// refusal exists. scripts/install/install_docker_host.py has no
	// platform gate at all — it refuses when the runner's account cannot
	// reach a daemon (exit 12) and merely STAGES the unit on a host with
	// no systemd. A gate satisfied by a mechanism that does not exist is
	// worse than no gate, because it certifies the sentence that sends
	// an operator looking for it. So the document has to name something
	// that really does refuse: the capability contract's answer, or the
	// engine's own refusal of a local step with no runner behind it.
	refusalMechanismRe = regexp.MustCompile(`(?i)capability contract|capabilities\.LocalHooks|LocalHooks\(|no host workflow runner|engine refuses|workflowrun/engine\.go`)
	// remoteInsteadRe: what the operator gets instead.
	remoteInsteadRe = regexp.MustCompile(`(?i)remote (workflow )?(hook|step|target)|over SSH`)
)

// LocalHookDocStates reports whether doc says what this provider's
// operator has to be told, given the answer that provider declares.
//
// The two branches ask for different things on purpose. A platform that
// CAN run the runner needs an install: the unit, the group grant and the
// image. A platform that cannot needs the opposite — that local hooks
// are unavailable, which mechanism refuses them, and what works in
// their place.
//
// Each branch also FORBIDS the other's headline sentence, and that half
// is what makes this a gate rather than a checklist. Without it the
// check was one-directional: flipping a contract row to `available`
// while its operator procedure still said "local workflow hooks are
// unavailable on this platform" left every requirement satisfied — the
// three install facts can perfectly well sit in a document that then
// tells the operator not to bother — so the one reader who matters would
// have been told the opposite of the truth by a green build.
func LocalHookDocStates(path, doc, answer string) (bool, string) {
	type requirement struct {
		re   *regexp.Regexp
		what string
	}
	var required []requirement
	var forbidden []requirement
	switch answer {
	case LocalHooksAvailable:
		required = []requirement{
			{runnerUnitRe, "name the systemd unit the runner is installed as (retnd-workflow-runner.service, or the pre-#890 backupd-workflow-runner.service, which only a host installed before that rename still has)"},
			{dockerGroupRe, "say that the runner's account needs the Docker socket's group (the `usermod -aG` grant)"},
			{hookImageRe, "name the hook image local hooks run in, which has to be present because the runner refuses to pull one"},
		}
		forbidden = []requirement{
			{hooksUnavailableRe, "still tells the operator that local workflow hooks are unavailable on this platform, which is now the opposite of what the capability contract answers for it"},
		}
	case LocalHooksUnavailable:
		required = []requirement{
			{hooksUnavailableRe, "state that local workflow hooks are unavailable on this platform"},
			{refusalMechanismRe, "name the mechanism that actually refuses them (the capability contract's answer, or the engine's refusal of a local step with no host workflow runner) rather than asserting a refusal nothing performs"},
			{remoteInsteadRe, "name what the operator gets instead: a remote workflow step, which runs over SSH and needs no Docker on the NAS"},
		}
	default:
		return false, fmt.Sprintf("declares localHooks %q, want %q or %q", answer, LocalHooksAvailable, LocalHooksUnavailable)
	}
	var findings []string
	for _, r := range required {
		if !r.re.MatchString(doc) {
			findings = append(findings, "does not "+r.what)
		}
	}
	for _, f := range forbidden {
		if f.re.MatchString(doc) {
			findings = append(findings, f.what)
		}
	}
	if len(findings) > 0 {
		return false, fmt.Sprintf("%s %s", path, strings.Join(findings, "; and "))
	}
	return true, fmt.Sprintf("%s states the %s answer, the Docker prerequisite and the local-hook consequence", path, answer)
}

// LocalHookCell decides one provider's local-workflow-hooks conformance
// cell.
//
// It returns true only for a provider that has a runtime platform of its
// own, whose capability-contract row says local hooks may run there, and
// whose operator-visible document says how. Everything else is a false
// with the reason, which is what the matrix turns into UNSUPPORTED or
// NOT_APPLICABLE against the declaration — and into a FAILURE when the
// declaration claims otherwise.
func LocalHookCell(wr WorkflowRunner, contract map[string]string) (bool, string) {
	if wr.LocalHooks == "" {
		return false, "this provider declares no workflowRunner block, so nothing says whether local hooks can run on it"
	}
	if wr.Doc == "" {
		return false, "this provider's workflowRunner block names no operator-visible document, so an operator has nowhere to be told the answer"
	}
	if wr.Platform == "" {
		return false, fmt.Sprintf("this provider ships no runtime profile of its own, so it has no row in the capability contract: it selects the generic profile and inherits generic's answer, which is %q. The provider-visible half is its own document, %s.", contract["generic"], wr.Doc)
	}
	declared, ok := contract[wr.Platform]
	if !ok {
		return false, fmt.Sprintf("this provider names platform %q, which the capability contract in %s declares no local-hook answer for", wr.Platform, LocalHookContractFile)
	}
	if declared != wr.LocalHooks {
		return false, fmt.Sprintf("conformance.json says local hooks are %s for platform %q and the capability contract says %s; %s is the authority, so one of the two is stale", wr.LocalHooks, wr.Platform, declared, LocalHookContractFile)
	}
	if wr.LocalHooks != LocalHooksAvailable {
		return false, fmt.Sprintf("platform %q cannot host the Host Workflow Runner, so local hooks are refused rather than run; %s states that and names the remote alternative", wr.Platform, wr.Doc)
	}
	doc, err := os.ReadFile(Path(wr.Doc))
	if err != nil {
		return false, fmt.Sprintf("cannot read %s: %v", wr.Doc, err)
	}
	return LocalHookDocStates(wr.Doc, string(doc), wr.LocalHooks)
}

// RuleLocalHookMounts is a profile that advertises local workflow hooks
// and mounts nothing the engine could reach a runner through.
const RuleLocalHookMounts = "local-hook-mounts-missing"

// CheckLocalHookMounts is the rule issue #921 was filed for: a provider
// that declares local hooks AVAILABLE and ships a runtime profile of its
// own has to mount the three paths the engine reaches the runner through.
//
// Why it did not already exist is the interesting part, because three
// rules pass over this and each is right on its own terms.
// CheckRequiredMounts holds the five storage roles and treats
// HostPlaneRoles as known and optional, which it has to: a NAS store
// profile that deploys no runner has no business being told it is
// missing storage. CheckStackEquivalence compares an adapter's mounts to
// the canonical stack's and draws the same distinction since #881.
// And the local-workflow-hooks matrix cell compares three DECLARATIONS —
// the capability contract, conformance.json and the operator's document
// — which is what #877 built it to do.
//
// So "available" and "mounts none of them" was a combination nothing
// read together, and four profiles were in it: an administrator who
// followed the procedure, installed the runner and watched systemd call
// it active still had an engine with no socket to dial, no credential to
// present and no scripts to read. The capability was advertised and
// undeliverable, which is the failure #877's row exists to prevent, one
// layer below where that row looks.
//
// Optionality is unchanged for everybody else. A provider that declares
// the hooks UNAVAILABLE is not asked (ZimaOS mounts none of the three and
// is right not to), and neither is the one that deploys
// container/compose.yaml itself, because that file carries them and
// "canonical-compose" is exactly the declaration that says so.
//
// What this must NOT key on is wr.Platform, and getting that wrong is
// how the rule would have missed half of #921. An empty Platform means
// no row in the capability contract — the provider selects the generic
// runtime profile and inherits generic's answer — and it says nothing
// about whether the provider ships a stack. CasaOS and Portainer have no
// platform row and ship a compose file each, and they were two of the
// four. So the question is whose runtime definition this is, which is
// what Metadata.Kind answers.
func CheckLocalHookMounts(source string, wr WorkflowRunner, kind string, engine *Service, c Canonical) []Violation {
	if wr.LocalHooks != LocalHooksAvailable || kind == "canonical-compose" {
		return nil
	}
	if engine == nil {
		return []Violation{{source, RuleLocalHookMounts,
			fmt.Sprintf("declares local hooks %s and no service running the engine command, so there is nothing for the three runner paths to be mounted into", LocalHooksAvailable)}}
	}

	readOnly := map[string]bool{}
	for _, p := range c.ReadOnlyContainerPaths {
		readOnly[p] = true
	}
	byRole := map[string]Mount{}
	for _, m := range engine.Mounts {
		byRole[m.Role] = m
	}

	var out []Violation
	for _, role := range HostPlaneRoles {
		want, _ := c.ContainerPaths.ByRole(role)
		m, ok := byRole[role]
		if !ok {
			out = append(out, Violation{source, RuleLocalHookMounts,
				fmt.Sprintf("service %s mounts nothing at %s, and this provider declares local hooks %s: the engine reads %s to reach the Host Workflow Runner, so an operator who installs the runner exactly as this provider's procedure says still gets every local step refused",
					backquote(engine.Name), backquote(want), LocalHooksAvailable, backquote(role))})
			continue
		}
		if readOnly[m.ContainerPath] && !m.ReadOnly {
			out = append(out, Violation{source, RuleLocalHookMounts,
				fmt.Sprintf("service %s mounts %s writable, and canonical.json declares it read-only", backquote(engine.Name), backquote(m.ContainerPath))})
		}
		if !readOnly[m.ContainerPath] && m.ReadOnly {
			out = append(out, Violation{source, RuleLocalHookMounts,
				fmt.Sprintf("service %s mounts %s read-only, and the canonical contract needs it writable: the runner's socket appears in that directory", backquote(engine.Name), backquote(m.ContainerPath))})
		}
	}
	sortViolations(out)
	return out
}

// ---------------------------------------------------------------------
// The host-side prerequisite, in canonical.json
// ---------------------------------------------------------------------

// WorkflowRunnerContract is canonical.json's `workflowRunner` block: the
// host-side facts every provider's documentation and every installer
// mention has to agree on, in the one file that already is the single
// source of truth for what the packaging profiles share.
//
// It is here because #865 introduced a version-pinned dependency that no
// packaged artifact carries. The hook image is named in three places —
// core/internal/hostrunner's DefaultHookImage, the installer's
// DEFAULT_HOOK_IMAGE and the unit's `--hook-image` — and the acceptance
// procedures name it a fourth time. Two of those were already pinned to
// each other; this makes the packaging manifests part of the same pin
// rather than a fifth independent copy.
type WorkflowRunnerContract struct {
	// Unit is the systemd unit the runner is supervised as.
	Unit string `json:"unit"`
	// Installer is what provisions it, relative to the repository root.
	Installer string `json:"installer"`
	// HookImage is the pinned image every local hook runs in.
	HookImage string `json:"hookImage"`
	// HookImageEnvKey and ModeEnvKey are how an operator names another
	// image and turns the runner off, in the deployment's .env.
	HookImageEnvKey string `json:"hookImageEnvKey"`
	ModeEnvKey      string `json:"modeEnvKey"`
	// EngineDockerAccess records whether the ENGINE container gets any
	// access to the Docker daemon. It is false, it has to stay false,
	// and it is recorded rather than assumed so that a change is an edit
	// to this file with a reviewer attached.
	EngineDockerAccess bool `json:"engineDockerAccess"`
}

// Where each of those values is actually decided. A canonical.json entry
// that nothing is held to is decoration, so every field above has an
// authority here and a test that reads it.
const (
	HookImageAuthorityFile = "core/internal/hostrunner/container.go"
	RunnerInstallerFile    = "scripts/install/install_docker_host.py"
)

var (
	// hookImageDefaultRe reads Go's `const DefaultHookImage = "..."`.
	hookImageDefaultRe = regexp.MustCompile(`DefaultHookImage\s*=\s*"([^"]+)"`)
	// pythonConstRe reads the installer's own module-level constants.
	pythonConstRe = regexp.MustCompile(`(?m)^([A-Z_]+)\s*=\s*"([^"]*)"`)
)

// ReadHookImageAuthority returns the hook image reference the runner
// itself defaults to.
func ReadHookImageAuthority() (string, error) {
	data, err := os.ReadFile(Path(HookImageAuthorityFile))
	if err != nil {
		return "", fmt.Errorf("packaging: read the hook image authority: %w", err)
	}
	m := hookImageDefaultRe.FindStringSubmatch(string(data))
	if m == nil {
		return "", fmt.Errorf("packaging: %s no longer declares `DefaultHookImage = \"...\"`, which canonical.json's workflowRunner.hookImage is pinned to", HookImageAuthorityFile)
	}
	return m[1], nil
}

// ReadInstallerConstants returns the runner-related string constants the
// installer declares, keyed by name.
func ReadInstallerConstants() (map[string]string, error) {
	data, err := os.ReadFile(Path(RunnerInstallerFile))
	if err != nil {
		return nil, fmt.Errorf("packaging: read the runner installer: %w", err)
	}
	matches := pythonConstRe.FindAllStringSubmatch(string(data), -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("packaging: %s declares no module-level string constants", RunnerInstallerFile)
	}
	out := make(map[string]string, len(matches))
	for _, m := range matches {
		out[m[1]] = m[2]
	}
	return out, nil
}

// ---------------------------------------------------------------------
// The submission rule #865 created the room for
// ---------------------------------------------------------------------

// RuleContainerDockerAccess is what fires when a shipped package would
// give a CONTAINER access to the Docker daemon.
const RuleContainerDockerAccess = "container-docker-access"

var (
	// groupAddRe is Compose's own way to put a container in a host
	// group, and `--group-add` is the docker-run and Unraid-template
	// spelling of the same thing. Neither was reachable by any existing
	// rule: ProhibitedHostPaths reads MOUNTS, and a group grant needs no
	// mount to hand over the daemon.
	groupAddRe = regexp.MustCompile(`(?m)(^\s*group_add\s*:|--group-add\b)`)
	// dockerClientEnvRe is the other half: an engine told where a daemon
	// is. DOCKER_HOST over TCP needs no socket and no group at all.
	dockerClientEnvRe = regexp.MustCompile(`\b(DOCKER_HOST|DOCKER_CONTEXT|DOCKER_TLS_VERIFY|DOCKER_CERT_PATH)\b`)
)

// CheckNoContainerDockerAccess refuses a packaged artifact that hands a
// container the Docker daemon.
//
// This is the drift #865 made possible and nothing else in this package
// could see. Local hooks now need a daemon, and the shortest way to
// "fix" a platform where they are unavailable is to give the engine
// container what the runner has: `group_add: [docker]`, or a
// `DOCKER_HOST` pointing at one. Both are root-equivalent on a NAS, both
// leave every other hardening key in the file untouched and therefore
// every other rule green, and both would be shipped through a store
// review that had been told the opposite (docs/submission/
// permission-rationale.md). The engine gains nothing; this is the check
// that makes that sentence load-bearing.
func CheckNoContainerDockerAccess(path, text string) []Violation {
	var out []Violation
	add := func(detail string) { out = append(out, Violation{path, RuleContainerDockerAccess, detail}) }

	if groupAddRe.MatchString(text) {
		add("puts a container in a host group (`group_add` / `--group-add`). The only group worth adding on a NAS is the Docker socket's, which is root-equivalent: it belongs to the host workflow runner, never to a shipped container (docs/runtime-contract.md, issue #865)")
	}
	if dockerClientEnvRe.MatchString(text) {
		add("sets a Docker client environment variable (`DOCKER_HOST` and friends), which hands a container a daemon to talk to without needing a socket mount at all. Local workflow hooks are the host runner's job; the engine container is never a Docker client")
	}
	return out
}
