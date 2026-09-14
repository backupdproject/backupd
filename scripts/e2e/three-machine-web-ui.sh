#!/usr/bin/env bash
# HELP-START
# Three machines on private networks, a real deployment, and a browser that
# has to talk to it over the wire.
#
# # The blind spot this closes
#
# suites/web-ui over in backupdproject/backupd-tests starts `npm run dev`
# and the app it drives resolves createMockApi out of
# ui/shared/src/api/mock.ts. So every case in that suite is a claim about a
# component rendering correctly GIVEN a fixture, and not one of them can go
# red on anything in the path a user actually meets:
#
#     browser -> serve-ui -> reverse proxy -> serve -> SQLite
#
# That is not a theoretical gap. The Activity page errored in the browser on
# a real NAS while the server answered HTTP 200 with valid JSON in twenty
# milliseconds, and it went unnoticed, because nothing that watches the
# server can see it and nothing that mocks the client can either: a mock
# answers with the shape the client already expects. This script stands up
# the whole path so a browser can fail on it.
#
# # The three machines
#
#   CLIENT    the browser and the Playwright runner, both inside the
#             container (scripts/e2e/client-machine.Dockerfile). It is on
#             the edge network and nothing else, so the only thing it can
#             reach is the product's front door.
#
#   RCLONE-MANAGER  the real product image, built from THIS working tree,
#             running the deployment container/compose.yaml describes.
#
#   VPS       a real sshd holding real files (atmoz/sftp, through
#             scripts/e2e/source-machine.Dockerfile, the same definition
#             two-machine-backup.sh and core/tests/machines use), playing
#             the machine being backed up.
#
# Three machines, four containers, and the difference is worth being plain
# about. The product IS two containers: `/backupd-web serve` (the engine: local
# authentication, /api/v1, the scheduler, SQLite) and `/backupd-web serve-ui`
# (the static bundle plus a reverse proxy to the engine). Collapsing them
# into one would delete the reverse-proxy hop, and that hop is half of what
# this suite exists to cover. So "the backupd machine" here is the
# product's own split, unchanged.
#
# # Why this drops docker-in-docker, and what that gives up
#
# scripts/e2e/two-machine-backup.sh runs its manager machine as
# docker-in-docker and installs onto it with the real installer. That is
# the right shape for what it proves, and its own comment says why this one
# cannot borrow it: the dind daemon is a network namespace of its own, so
# 8080 in there is not 8080 out here, and a sibling client container has no
# route to it at all. The two ways out were to publish the inner port back
# out of the dind container onto the shared network, or to drop dind and
# run the product image directly on the network. This takes the second.
#
# Publishing back out would work, and what it would produce is a browser
# talking to a port forwarded by a daemon running inside a daemon, which is
# a hop that exists nowhere in production. Every timeout and every reset on
# it would be a question about the test rig. The thing under test here is
# an HTTP path, so the fewer inventions between the browser and the server
# the better.
#
# What that gives up is stated rather than quietly dropped: THIS SCRIPT
# DOES NOT TEST THE INSTALLER. It tests the running product.
# two-machine-backup.sh remains the test that scripts/install/
# install_docker_host.py takes a bare machine to a working deployment, and
# it must stay that way. A green run here says nothing about whether an
# operator can install this.
#
# # Three networks, not one
#
#     client ──edge── web-ui ──internal── engine ──backhaul── vps
#
# One flat network would have been fewer lines. It would also let a spec
# reach the engine directly and pass while proving nothing about the proxy,
# and it would put the client on the same wire as an engine that runs with
# TRUST_FORWARDED_HEADERS=true, which container/compose.yaml goes to some
# length to explain is only safe because serve-ui is its sole possible
# peer. Both of those are the topology doing work, so the topology is the
# real one. Nothing is published to the host on any of them.
#
# The name `backupd` lives on the edge network only, and it resolves
# to the UI container: from outside the deployment that IS backupd,
# and it is the address the browser is given. Inside the deployment the
# containers keep compose's own names. One name per network, so nothing
# ever resolves to two things.
#
# # Seeded state, generated per run, committed never
#
# The VPS holds three files of known content. The deployment is configured
# with a backup set pointing at it over SSH, a cycle is run so the journal
# and the catalogue have real rows in them, and an administrator is
# enrolled. The SSH keypair is generated INSIDE a container and lives in a
# Docker volume that is destroyed at teardown, so the private half never
# touches this host's filesystem. The administrator's password is generated
# per run, reaches the product down a pipe into stdin, and is printed here
# because a suite that has to sign in needs it. Both are dead the moment
# this script returns.
#
# # Handing it to a suite
#
# The client container gets these, and they are the whole contract:
#
#   RM_BASE_URL          http://backupd:8080
#   RM_ADMIN_USERNAME    the enrolled administrator
#   RM_ADMIN_PASSWORD    its password, generated this run
#   RM_BACKUP_SET        the seeded set's name
#   RM_ARTIFACTS_DIR     /artifacts, mounted out to the host
#
# plus, unless --no-workflows, the workflow contract in the section
# below: the backup sets #816's scenarios need, the seeded script
# library's root, and the control channel that crashes the engine or
# takes the runner away.
#
# A Playwright config that sees RM_BASE_URL must use it as `baseURL` and
# must NOT start a web server: there is one, it is another container, and
# `npm run dev` in here would serve the mock this whole script exists to
# get away from.
#
# # Running it
#
#   scripts/e2e/three-machine-web-ui.sh
#       stand the stack up and run the built-in browser check.
#
#   scripts/e2e/three-machine-web-ui.sh --suite ../backupd-tests/suites/web-ui
#       stand it up and run that directory's Playwright suite inside the
#       client container. The suite's own node_modules is not used: the
#       image's is, because the checkout's was built for this host.
#
#   scripts/e2e/three-machine-web-ui.sh --keep-up
#       stand it up, print how to drive it, and leave it running. For
#       working on the suite without paying the setup cost per attempt.
#       Tear it down afterwards with the line it prints.
#
#   --artifacts DIR   where traces, screenshots and reports land. Outside
#                     this repository by default, and never removed by the
#                     teardown, because a failing run whose evidence was
#                     deleted with the stack is worse than no evidence.
#   --image REF       skip the build and use an already-built product image.
#   --keep-on-failure leave a failed stack up for reading.
#   --front-proxy-tls put an ordinary TLS + HTTP/2 reverse proxy in front of
#                     serve-ui, so the browser reaches the stack the way a
#                     real NAS's front door does (h2 over TLS) rather than
#                     the plain HTTP/1.1 this rig otherwise uses. The
#                     reproduction for backupd#730. RM_SEED_CYCLES=N
#                     additionally runs N backup cycles to enlarge the feed.
#   --break-engine    hand the suite the ability to take the ENGINE away
#                     mid-session, leaving serve-ui up, so the browser
#                     meets a front door that cannot reach the service
#                     behind it. The reproduction for backupd#795.
#
#                     The engine is NOT stopped up front, and that is the
#                     whole design. serve-ui proxies all of /api/v1,
#                     /auth/session included, so a stack that starts
#                     broken never gets a browser past the login page and
#                     the Activity page is never reached. The reported NAS
#                     failed the other way round: a loaded, signed-in app
#                     whose engine went away underneath it. So the break
#                     happens while the suite holds a live session.
#
#                     The client container gets two more variables:
#
#                       RM_ENGINE_UNREACHABLE=1   branch on this
#                       RM_ENGINE_CONTROL         a directory under
#                                                 /artifacts, described
#                                                 below
#
#                     and a watcher on THIS host owns the docker socket
#                     the client deliberately does not have. The protocol
#                     is files in that directory:
#
#                       write "stop"    -> the engine container is stopped
#                                          and "stopped" appears, or
#                                          "stop-failed" does
#                       write "start"   -> it is started, and "started"
#                                          appears only once its own
#                                          healthcheck passes, or
#                                          "start-failed" does
#
#                     Exactly one ack appears per request, and a success
#                     ack means the state was VERIFIED: "stopped" only
#                     after Docker reports the container not running,
#                     "started" only after `docker start` succeeded and
#                     the engine's own healthcheck passed. Anything else
#                     is a failure ack carrying a one-line reason, which
#                     the suite quotes rather than reporting its own
#                     timeout. A suite that reads a success ack as proof
#                     of the state is reading it correctly, which is why
#                     one is never written on a guess (#795).
#
#                     A request file is removed as it is picked up, so one
#                     request is never acknowledged by the leavings of the
#                     last, and "start" against an engine that is already
#                     running is a no-op that still acknowledges. A suite
#                     that deletes an ack before asking again is doing the
#                     right thing and is expected to.
#
#                     Not combinable with --keep-up: the watcher is a
#                     process of THIS script, so a kept-up stack has
#                     nobody acking. The two variables are then left off
#                     the printed command on purpose, and the manual
#                     equivalent is printed instead.
#
#                     Before handing over, the break is REHEARSED: the
#                     engine is stopped, the edge network is asked for
#                     /api/v1/activity and has to come back 502 with an
#                     X-Correlation-Id on it, and the engine is started
#                     again. A mode that cannot demonstrate the fault it
#                     exists to produce fails here rather than handing a
#                     suite a healthy stack to pass against.
#
# # The workflow machinery (#816)
#
# EPIC L put hook scripts in the product, and none of them can be proven
# in a browser against a mock: a local hook is executed by a SEPARATE
# PROCESS on the host, a remote one by an sshd that may refuse, and the
# interesting states are what a run looks like when one of those fails
# halfway. So the rig stands the whole apparatus up, and by default
# rather than behind a flag, because a workflow case that skips itself
# for want of a fixture is a case that never ran.
#
#   THE RUNNER    `backupd workflow-runner serve`, from THIS run's
#                 product image, in a container of its own
#                 (scripts/e2e/runner-machine.Dockerfile) that is NOT
#                 the engine's. It holds this host's Docker socket,
#                 because every local hook runs in an ephemeral
#                 container (#865) -- which is the privilege the
#                 installer grants the runner's service account and
#                 withholds from the engine. The engine reaches it
#                 through one authenticated Unix socket in a volume the
#                 two share, with the credential read at the path the
#                 ENGINE sees (backupd#877).
#
#   THE EXEC HOST a second sshd (scripts/e2e/exec-host.Dockerfile, the
#                 definition core/tests/machines already uses), carrying
#                 an ordinary shell account and an internal-sftp-forced
#                 one that authenticate with the SAME client key. That is
#                 what makes "this credential may not exec" provable:
#                 e2e/workflow-remote runs a NAME.remote.sh over the
#                 shell account and e2e/vps is refused one over its own
#                 transfer credential, by the server, not by this rig.
#
#   THE LIBRARY   scripts/e2e/workflows/, seeded into a volume the engine
#                 mounts read-only at /workflows. One stage directory per
#                 scenario, and every line those scripts print is
#                 asserted by the suite, which is why they are committed
#                 files rather than here-documents.
#
# The client container gets a backup set name per scenario -- happy,
# remote, SFTP-only-refused, no-hooks, before-fail, after-fail, hostile
# output, secret-backed environment, slow, crash, many-step, lint
# findings and a gate-refused script -- and the plaintext of the one
# secret a hook resolves, so a spec can assert it appears NOWHERE.
#
#   RM_WORKFLOW_CONTROL  a directory under /artifacts, the same protocol
#                        --break-engine's channel uses and for the same
#                        reason: the capability lives on THIS host and
#                        the client is given a few files rather than a
#                        Docker socket. Three requests:
#
#                          crash        -> the engine is KILLED and
#                                          started again, so a run in
#                                          flight is one nobody observed
#                                          the end of and startup
#                                          reconciliation has to account
#                                          for it. "crashed" appears
#                                          once the browser's own path to
#                                          the engine is good again, or
#                                          "crash-failed" does.
#                          runner-down  -> the runner is stopped, so a
#                                          local hook has nothing to run
#                                          on. "runner-stopped", or
#                                          "runner-down-failed".
#                          runner-up    -> and started again, acked
#                                          "runner-started" only once it
#                                          answers its own status verb,
#                                          or "runner-up-failed".
#
#                        Exactly one ack per request, a success ack means
#                        a state this script WATCHED the stack reach, and
#                        the request file is removed as it is picked up.
#                        Not handed over under --keep-up, for the reason
#                        the engine channel is not.
#
#   --no-workflows   leave all of it out. For a machine whose daemon
#                    cannot host the runner, and it is honest rather than
#                    quiet: none of the variables above is set, so every
#                    workflow case skips itself and says so.
#
# One thing this rig cannot give a browser, and says so rather than
# leaving a suite to discover it as a timeout: a browser cannot START a
# backup. POST /api/v1/operations run_backup_set is refused 403
# DESTRUCTIVE_OPERATIONS_DISABLED on every deployment this repository
# can build, because apps/common/webhost ships one DestructiveGate and
# its own doc says nothing may flip it before #92. The SCHEDULER runs
# the same cycle on a timer regardless, which gate.go states in as many
# words, so the rig sets that timer to the product's own floor -- one
# minute, config.MinPollInterval -- and hands it over as
# RM_WF_POLL_SECONDS. A spec watches for the run the timer produces
# instead of asking for one.
#
# The exit status is the client container's, not the teardown's. A run that
# tore down cleanly after a red suite is a red run.
#
# # Cost
#
# About forty seconds of setup on the machine this was written on once the
# product image is built, and the product image build is minutes on a cold
# cache because it compiles the Go binaries and the UI bundle. --image and
# --keep-up both exist to stop paying that per attempt.
# HELP-END

set -euo pipefail

# --help reads this file and the run cd's to the repository root, so both
# paths are settled here, from the same dirname, before that happens. Same
# reasoning as two-machine-backup.sh, and the same shape.
self="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$repo_root"

# ---------------------------------------------------------------- output

step() { echo ""; echo "==> three-machine: $*"; }
note() { echo "    $*"; }

die() {
  echo "" >&2
  echo "==> three-machine: FAILED. $1" >&2
  shift
  for line in "$@"; do echo "    $line" >&2; done
  exit 1
}

# A capability this machine does not have is not a failure and is not a
# pass either. It leaves as 3, which is what scripts/lib/ci-local-gate.sh
# reads as INCOMPLETE.
#
# It travels to `finish` as its own number rather than as 3, for the reason
# two-machine-backup.sh spells out at length: the CLI under test has its own
# meaning for exit 3 (#551, another process is already serving this
# deployment), several commands below are that CLI, and a run that met that
# refusal must not be reported as a machine that never tried.
EXIT_CANNOT_RUN=97
cannot_run() {
  echo "" >&2
  echo "==> three-machine: CANNOT RUN. $1" >&2
  shift
  for line in "$@"; do echo "    $line" >&2; done
  exit "$EXIT_CANNOT_RUN"
}

# ------------------------------------------------------------------ help
#
# The block between the two markers at the top of this file, never a range
# of line numbers (#514): markers move with the text they delimit, and a
# comment inserted above one leaves the rendered help unchanged.
render_help() {
  awk '
    /^# HELP-END$/   { closed = 1; inside = 0; next }
    /^# HELP-START$/ { opened = 1; inside = 1; next }
    inside           { sub(/^# ?/, ""); print }
    END              { if (!opened || !closed) exit 1 }
  ' "$self" || die "the help block is missing from $self" \
    "--help renders the lines between the HELP-START and HELP-END markers," \
    "and this file has lost one or both of them."
}

# --------------------------------------------------------------- options

suite_dir=""
artifacts_dir=""
keep_on_failure=0
keep_up=0
prebuilt_image="${RM_PRODUCT_IMAGE:-}"
# backupd#730 reproduction: put an ordinary TLS + HTTP/2 reverse
# proxy in front of serve-ui, so the browser reaches the stack the way it
# reaches a real NAS (h2 over TLS) rather than the plain HTTP/1.1 the rig
# otherwise uses. Off by default; the default rig is unchanged.
front_proxy="${RM_FRONT_PROXY_TLS:-0}"
# backupd#795 reproduction: let the suite take the engine away while the
# browser holds a live session, with serve-ui left up in front of it.
# Off by default; every line it adds is behind this flag, so a default run
# is the run it was before.
break_engine="${RM_BREAK_ENGINE:-0}"
# EPIC L (#816). The Host Workflow Runner, the exec host, the seeded
# script library and the backup sets that use them. ON by default,
# because the point of this rig is that a browser meets the real thing:
# a suite handed a deployment with no hooks configured skips every
# workflow case and reports a pass.
workflows="${RM_WORKFLOWS:-1}"

while [ $# -gt 0 ]; do
  case "$1" in
    --suite) suite_dir="${2:-}"; shift 2 ;;
    --suite=*) suite_dir="${1#--suite=}"; shift ;;
    --artifacts) artifacts_dir="${2:-}"; shift 2 ;;
    --artifacts=*) artifacts_dir="${1#--artifacts=}"; shift ;;
    --image) prebuilt_image="${2:-}"; shift 2 ;;
    --image=*) prebuilt_image="${1#--image=}"; shift ;;
    --keep-on-failure) keep_on_failure=1; shift ;;
    --keep-up) keep_up=1; shift ;;
    --front-proxy-tls) front_proxy=1; shift ;;
    --break-engine) break_engine=1; shift ;;
    --no-workflows) workflows=0; shift ;;
    -h|--help) render_help; exit 0 ;;
    *) die "unknown option $1" \
           "Usage: $0 [--suite DIR] [--artifacts DIR] [--image REF] [--front-proxy-tls] [--break-engine] [--no-workflows] [--keep-up] [--keep-on-failure]" ;;
  esac
done

if [ -n "$suite_dir" ]; then
  [ -d "$suite_dir" ] || die "--suite $suite_dir is not a directory."
  suite_dir="$(cd "$suite_dir" && pwd)"
  [ -f "$suite_dir/playwright.config.ts" ] || [ -f "$suite_dir/playwright.config.js" ] \
    || die "--suite $suite_dir has no playwright.config.ts in it." \
           "That is the directory the client container runs \`npx playwright test\` in, so it has to be the suite root."
fi

# ------------------------------------------------------------ identities

run_id="${E2E_RUN_ID:-$$-$(date +%s)-${RANDOM}}"
label="backupd-e2e=three-machine-web-ui"

net_edge="rm-webui-edge-$run_id"
net_internal="rm-webui-internal-$run_id"
net_backhaul="rm-webui-backhaul-$run_id"

c_source="rm-webui-vps-$run_id"
c_engine="rm-webui-engine-$run_id"
c_web="rm-webui-web-$run_id"
c_client="rm-webui-client-$run_id"
c_proxy="rm-webui-proxy-$run_id"
# The two machines EPIC L adds. The runner is the process the engine
# cannot be, and the exec host is the machine a remote hook runs on.
c_exec="rm-webui-exechost-$run_id"
c_runner="rm-webui-runner-$run_id"

# The deployment's state, in Docker volumes rather than host directories,
# and the reason is the SSH key. core/internal/transport/rclone/ssh.go
# refuses a key_file whose mode is not exactly 0600 and refuses any
# ancestor directory that is group- or world-writable, which is the right
# rule and is also the rule a bind mount cannot reliably satisfy: file
# ownership across a Docker Desktop share is not the host's, and a mode
# that survives on Linux does not survive there. A volume is the same
# filesystem the container writes with, so 0600 means 0600. It also means
# the private key never lands on this host at all.
v_keys="rm-webui-keys-$run_id"
v_config="rm-webui-config-$run_id"
v_state="rm-webui-state-$run_id"
v_backups="rm-webui-backups-$run_id"
# The workflow machinery's three, and each is a volume rather than a host
# directory for the reason v_keys is: modes and ownership. The script
# library has to be owned by the uid the engine runs as and not
# group-writable or the engine refuses to read it; the runner's socket
# directory has to be 0700 owned by the runner; and the credential in the
# secrets volume has to be exactly 0600 or the runner refuses to load it.
# A Docker volume is the same filesystem both containers see, so a mode
# set once is the mode both get.
v_workflows="rm-webui-workflows-$run_id"
v_wfrun="rm-webui-wfrun-$run_id"
v_wfsecrets="rm-webui-wfsecrets-$run_id"

# The payload is the exception, and deliberately: the harness generates it
# and has to be able to digest it from outside, and it holds nothing
# secret. It lives in a run directory OUTSIDE this repository, so nothing
# this script writes can be committed by accident.
tmp_root="${TMPDIR:-/tmp}"
tmp_root="${tmp_root%/}/backupd-e2e-web-ui"
run_dir="$tmp_root/$run_id"

# Artifacts outlive the stack on purpose, so this is not $run_dir. Outside
# the repository by default because a Playwright trace of a sign-in carries
# the password that was typed into it, and this repository is not where
# credential material goes, dead or not.
[ -n "$artifacts_dir" ] || artifacts_dir="$tmp_root/$run_id-artifacts"

# backupd#795's control channel, and it lives UNDER the artifacts
# directory rather than beside it for one reason: that directory is
# already bind-mounted into the client container, and the client must not
# be given anything else. A suite that could reach the Docker socket could
# stop the engine itself, and would then be a suite that can do anything
# to this host; a few small files in a directory it already has is the
# whole capability it needs.
engine_control="$artifacts_dir/engine-control"
engine_control_in_client="/artifacts/engine-control"
engine_watcher_pid=""

# #816's control channel, beside the engine's and for the same reasons.
# Three verbs rather than two, because the states this one has to produce
# are a crashed engine and an absent runner.
workflow_control="$artifacts_dir/workflow-control"
workflow_control_in_client="/artifacts/workflow-control"
workflow_watcher_pid=""

# The runner's workspace, which is the one path in this rig that must be
# a HOST directory: the runner asks the daemon to bind-mount each step's
# working directory into that step's hook container, and the daemon
# resolves those paths on the host. So the runner's own view of them has
# to be the host's, which is why it is mounted at the same path inside
# the runner's container rather than at a tidier one.
#
# Beside the artifacts directory rather than inside $run_dir, because
# $run_dir is 0700 owned by whoever ran this and the runner runs as the
# deployment's uid: a 0700 ancestor owned by somebody else is a
# directory it cannot traverse at all.
wf_prefix="$tmp_root/$run_id-workflow-runner"
wf_workspace="$wf_prefix/workspace"

product_image="${prebuilt_image:-backupd-web-ui-e2e:$run_id}"
source_image="backupd-e2e-source:1"
client_image="backupd-e2e-client:1"
proxy_image="backupd-e2e-proxy:1"
exec_image="backupd-e2e-exec-host:1"
# Per-run, because it is built FROM the product image this run tested:
# the runner refuses an engine from another release, so the tag has to
# move when that image does.
runner_image="backupd-e2e-runner:$run_id"
# The image every local hook runs in. `workflow-runner serve` refuses to
# pull one, so this is pulled by the rig, once, and is the same reference
# distribution/packaging/canonical.json pins for a real installation.
hook_image="${RM_HOOK_IMAGE:-bash:5.2.37-alpine3.21}"

source_dockerfile="$repo_root/scripts/e2e/source-machine.Dockerfile"
client_dockerfile="$repo_root/scripts/e2e/client-machine.Dockerfile"
proxy_dockerfile="$repo_root/scripts/e2e/proxy-machine.Dockerfile"
exec_dockerfile="$repo_root/scripts/e2e/exec-host.Dockerfile"
runner_dockerfile="$repo_root/scripts/e2e/runner-machine.Dockerfile"
if [ "$workflows" = 1 ]; then
  [ -r "$exec_dockerfile" ] || die "the exec host Dockerfile is missing at $exec_dockerfile."
  [ -r "$runner_dockerfile" ] || die "the workflow runner machine Dockerfile is missing at $runner_dockerfile."
  [ -d "$repo_root/scripts/e2e/workflows" ] \
    || die "the seeded hook script library is missing at $repo_root/scripts/e2e/workflows." \
           "Every workflow case asserts a line one of those scripts prints, so there is nothing to seed without it."
fi
[ -r "$source_dockerfile" ] || die "the VPS machine Dockerfile is missing at $source_dockerfile."
[ -r "$client_dockerfile" ] || die "the client machine Dockerfile is missing at $client_dockerfile."
[ "$front_proxy" != 1 ] || [ -r "$proxy_dockerfile" ] \
  || die "the front-proxy machine Dockerfile is missing at $proxy_dockerfile."

sftp_user="backupuser"
sftp_uid=1001

# The uid/gid the product runs as, matching container/compose.yaml's own
# PUID/PGID defaults. The volumes above are chowned to it before anything
# starts, because a distroless image has no shell and no root step to fix
# an ownership problem from the inside.
app_uid=1000
app_gid=1000

backup_set="e2e/vps"

# #816's scenarios, one backup set each, and the names are the contract
# the suite reads out of the environment. Each gets its own local path so
# no two of them write over each other, and none is scheduled: they run
# when a browser asks for a run, which is the thing under test.
wf_set_happy="e2e/workflow-happy"
wf_set_remote="e2e/workflow-remote"
wf_set_none="e2e/workflow-none"
wf_set_before_fail="e2e/workflow-before-fail"
wf_set_after_fail="e2e/workflow-after-fail"
wf_set_hostile="e2e/workflow-hostile"
wf_set_secret="e2e/workflow-secret"
wf_set_slow="e2e/workflow-slow"
wf_set_crash="e2e/workflow-crash"
wf_set_many="e2e/workflow-many"
wf_set_findings="e2e/workflow-findings"

# name:local-path-under-/data/backups. e2e/vps is not here: it exists
# already, it is the SFTP-only posture, and its cycle has to run before
# any hook is attached to it.
wf_sets=(
  "$wf_set_happy:workflow-happy"
  "$wf_set_remote:workflow-remote"
  "$wf_set_none:workflow-none"
  "$wf_set_before_fail:workflow-before-fail"
  "$wf_set_after_fail:workflow-after-fail"
  "$wf_set_hostile:workflow-hostile"
  "$wf_set_secret:workflow-secret"
  "$wf_set_slow:workflow-slow"
  "$wf_set_crash:workflow-crash"
  "$wf_set_many:workflow-many"
  "$wf_set_findings:workflow-findings"
)

# set|before-dir|after-dir|execution-connection, all four fields
# present and the empty ones meaning "this stage is disabled", which is a
# different state from a stage that exists and is empty. Relative to
# /workflows, the way `backup-set workflow patch` spells them.
#
# $wf_set_none is deliberately absent: a set with no workflow block at
# all is what the quiet empty surface renders, and a global stage would
# make that state unreachable for every set at once, which is why none is
# configured here either.
exec_connection="exec-host"
exec_user="hookuser"
wf_stages=(
  "$wf_set_happy|happy-before|happy-after|"
  "$wf_set_remote|remote-before|remote-after|$exec_connection"
  # The SFTP-only posture, and the fourth field is the whole of it. A
  # NAME.remote.sh with NO execution connection is not a refusal at all
  # -- the plan cannot even be built, and the product says so: "runs on
  # the host this backup set pulls from, and no execution connection is
  # configured for it". That is a configuration mistake, not #810's
  # case.
  #
  # #810's case is the operator who names the connection they already
  # have: a reference spelled source/set resolves to the backup set's
  # OWN transfer credential, which here is atmoz/sftp's chrooted,
  # internal-sftp-forced account. It authenticates, it transfers three
  # files a cycle, and the server answers an exec request with "This
  # service allows sftp connections only." So the hook is refused for
  # the reason the epic is about, by the sshd, while the backup over the
  # same credential goes on working.
  "$backup_set|sftp-only-before||$backup_set"
  "$wf_set_before_fail|before-fail-before||"
  "$wf_set_after_fail||after-fail-after|"
  "$wf_set_hostile||hostile-after|"
  "$wf_set_secret|secret-before||"
  "$wf_set_slow|slow-before||"
  "$wf_set_crash|crash-before|crash-after|"
  "$wf_set_many|many-before|many-after|"
  "$wf_set_findings|findings-before||"
)

# The gate's own fixture, and it is NOT in the list above, because the
# product refuses to let it be: `backup-set workflow patch` runs the
# shell verification over every script the change points at and declines
# the whole save at ERROR severity (L7.5, #906). rejected-before holds a
# BSH003 -- a recursive forced delete that becomes a root-level path when
# its expansion is empty -- so attaching it here fails, correctly, and a
# rig that worked around that would be a rig testing a product nobody
# ships.
#
# So the set exists with NO hooks configured and the directory is seeded
# and named to the suite instead. The refusal is then provable where it
# happens: a browser filling that directory into the set's workflow form
# and being turned down with the finding on it.
wf_rejected_dir="rejected-before"

# The names on the runner's side of the socket, and the paths on the
# ENGINE's. They are different views of the same two files, which is the
# whole of why config.yaml carries the engine's view: a token_file naming
# the installer's host path is a file that does not exist in there, and
# every local hook then fails authentication (backupd#877).
runner_socket_name="workflow-runner.sock"
runner_token_name="workflow-runner.token"
engine_secrets_mount="/etc/backupd/wf-secrets"
engine_token_path="$engine_secrets_mount/$runner_token_name"
# How the product names the runner on its own surfaces, handed to the
# suite so a spec asserting the "Runs on" column reads it from here
# rather than from a copy of the product's string.
runner_display="Host Workflow Runner"

# How often the deployment polls, in seconds, and it is the product's own
# floor (config.MinPollInterval): a browser cannot START a run -- the
# destructive gate refuses one on every deployment this repository can
# build (#92) -- so the runs a suite watches are the scheduler's, and
# this is how long the longest of those waits can be. Handed to the
# client so no spec has to hard-code a cadence the rig owns.
wf_poll_seconds=60

# The one secret a hook of this run resolves. Generated per run like the
# administrator's password, written into the deployment's secrets volume
# and never into config.yaml, which holds the PATH to it. It is handed to
# the client so a spec can assert it appears on no surface at all: the
# seeded hook prints only its length, so a page holding this string is a
# disclosure and not a fixture artefact.
wf_secret_env="WF_E2E_SECRET"
wf_secret_name="wf-e2e-secret"
wf_secret="e2e-secret-$(openssl rand -hex 12)"
engine_secret_path="$engine_secrets_mount/$wf_secret_name"

# What the client is told. `backupd` is an alias on the edge network
# and on no other, so it resolves to exactly one container from exactly one
# place, seen from the client: the UI container normally, or the TLS/HTTP-2
# front proxy when --front-proxy-tls is set (which then upstreams to the UI
# container, aliased `origin` on the same edge network).
# When the front proxy is in play the browser and any node fetch reach the
# stack over TLS with a self-signed leaf, so the probes and the client are
# told to accept it: this rig is exercising the h2/transport path, not
# certificate trust. front_proxy_probe_env is used UNQUOTED on purpose so
# the empty default expands to no argument at all.
if [ "$front_proxy" = 1 ]; then
  base_url="https://backupd"
  edge_web_alias="origin"
  front_proxy_probe_env="-e NODE_NO_WARNINGS=1 -e NODE_TLS_REJECT_UNAUTHORIZED=0"
else
  base_url="http://backupd:8080"
  edge_web_alias="backupd"
  front_proxy_probe_env=""
fi

# Generated here, printed below, never written to a file by this script. It
# reaches `auth create-admin` down a pipe into stdin and reaches the client
# container as an environment variable, which is what the product's own
# documentation prescribes for both. Twelve characters is the product's own
# minimum (the enrolment form enforces it), so this comfortably clears it.
admin_user="e2e-operator"
admin_pass="e2e-$(openssl rand -hex 16)"

# ------------------------------------------------------------- teardown

created_containers=()
created_networks=()
created_volumes=()
created_images=()
teardown_done=0

teardown() {
  local status="${1:-0}"
  [ "$teardown_done" = 1 ] && return
  teardown_done=1

  # The watcher first, and before the --keep-up return below rather than
  # after it: it is a process on THIS host holding the Docker socket, and
  # a run that leaves the stack up on purpose still must not leave a loop
  # behind that stops a container somebody is reading. Killed even when
  # the containers are kept.
  stop_engine_watcher
  stop_workflow_watcher

  # The hook containers, which are the one thing in this rig that
  # another process creates. The runner starts one per local hook and
  # removes it as the step ends, so a clean run leaves none; a run
  # killed mid-hook can, and a container holding a mount of a directory
  # this teardown is about to remove is how a run leaves rubbish behind.
  #
  # `backupd.workflow-hook=1` is the PRODUCT's label
  # (core/internal/hostrunner's LabelHook), not this rig's, and it
  # carries no rig identity -- so on its own that filter names every
  # hook container on the host, including a real deployment's runner's
  # and a second instance of this rig's. This used to say that could
  # only be this run's, which was an assumption about the host rather
  # than a property of the filter.
  #
  # `since` is the identity that is available: this rig's runner is
  # created before any hook it can possibly launch, so every hook of
  # THIS run is newer than that container and no other runner's is
  # caught. Every other container here is removed by a name this script
  # recorded; this is the one it cannot name in advance, and it is now
  # bounded the same way in spirit.
  if [ "$workflows" = 1 ] && [ -n "$c_runner" ]; then
    for h in $(docker ps -aq --filter "label=backupd.workflow-hook=1" --filter "since=$c_runner" 2>/dev/null); do
      docker rm -f "$h" >/dev/null 2>&1 || true
    done
  fi

  if [ "$keep_up" = 1 ] || { [ "$keep_on_failure" = 1 ] && [ "$status" != 0 ]; }; then
    echo "" >&2
    if [ "$keep_up" = 1 ]; then
      echo "==> three-machine: --keep-up, so the stack is still running:" >&2
    else
      echo "==> three-machine: --keep-on-failure, so the failed stack is left up for reading:" >&2
    fi
    for c in "${created_containers[@]:-}"; do [ -n "$c" ] && echo "        container $c" >&2; done
    for n in "${created_networks[@]:-}"; do [ -n "$n" ] && echo "        network   $n" >&2; done
    for v in "${created_volumes[@]:-}"; do [ -n "$v" ] && echo "        volume    $v" >&2; done
    echo "" >&2
    echo "    Tear it down with:" >&2
    echo "        docker rm -fv ${created_containers[*]:-} && docker network rm ${created_networks[*]:-} && docker volume rm ${created_volumes[*]:-}" >&2
    return
  fi

  echo ""
  echo "==> three-machine: tearing down"
  for c in "${created_containers[@]:-}"; do
    [ -n "$c" ] && docker rm -fv "$c" >/dev/null 2>&1 || true
  done
  # After the containers, never before: a network with an endpoint on it
  # cannot be removed, and "network is in use" at teardown time is how a
  # network survives a run.
  for n in "${created_networks[@]:-}"; do
    [ -n "$n" ] && docker network rm "$n" >/dev/null 2>&1 || true
  done
  # And the volumes after the networks, for the same reason one step
  # further out: a volume with a container still attached is not removable
  # either, and these hold the run's SSH private key.
  for v in "${created_volumes[@]:-}"; do
    [ -n "$v" ] && docker volume rm "$v" >/dev/null 2>&1 || true
  done
  for i in "${created_images[@]:-}"; do
    [ -n "$i" ] && docker image rm -f "$i" >/dev/null 2>&1 || true
  done
  remove_run_dir
  # $artifacts_dir is NOT removed here, and that is the point of it.
}

# remove_run_dir deletes what this run wrote, by name, and then removes the
# directories. Deliberately not a recursive delete: the files are a known,
# short list, and `rm -rf` on a path built from variables is how a script
# eventually deletes the wrong thing. An unexpected extra file leaves the
# directory behind, which is visible rather than silent.
remove_run_dir() {
  [ -d "$run_dir" ] || return 0
  rm -f "$run_dir/upload/payload.bin" "$run_dir/upload/schema.sql" "$run_dir/upload/notes.txt" 2>/dev/null || true
  rm -f "$run_dir/authorized_keys/engine.pub" "$run_dir/hookdata/marker.txt" 2>/dev/null || true
  # The runner's workspace, which is the one directory here this script
  # does not write itself: `workflow-runner serve` creates one per run
  # and one per step under it and removes each as its step ends, so a
  # clean run leaves an empty tree and a crashed one leaves whatever
  # that step had got to. Removed recursively, and the recursion happens
  # INSIDE a container whose only mount is that directory: the literal
  # /w/workflow cannot name anything else, which is the property the
  # by-name rule above is protecting.
  if [ -d "$wf_workspace" ]; then
    toolbox -v "$wf_workspace:/w" -- 'rm -rf /w/workflow' >/dev/null 2>&1 || true
    rmdir "$wf_workspace" "$wf_prefix" 2>/dev/null || true
  fi
  rmdir "$run_dir/upload" "$run_dir/authorized_keys" "$run_dir/hookdata" "$run_dir" 2>/dev/null || true
  rmdir "$tmp_root" 2>/dev/null || true
}

# Three traps, not one. A bash trap on INT or TERM runs the handler and then
# RESUMES the script, so `trap teardown EXIT INT TERM` would tear everything
# down and then carry on against containers that are no longer there. These
# exit, which fires the EXIT trap too; teardown_done makes the second call a
# no-op. The status is passed in rather than read from $?, because inside a
# signal handler $? is the status of whatever the signal interrupted.
finish() {
  local status="${1:-0}"
  teardown "$status"
  case "$status" in
    "$EXIT_CANNOT_RUN") exit 3 ;;
    3)
      echo "" >&2
      echo "==> three-machine: FAILED. A command exited 3." >&2
      echo "    This script reserves 3 for the gate's \"this machine could not perform the proof\" verdict and never" >&2
      echo "    produces it itself, so a 3 here came from something with its own meaning for that status: most likely" >&2
      echo "    the CLI refusing a configuration write because the engine is already serving this deployment (#551)." >&2
      echo "    Reported as a failure, which is what a proof that did not finish is." >&2
      exit 1 ;;
    *) exit "$status" ;;
  esac
}
trap 'finish $?' EXIT
trap 'teardown 130; exit 130' INT
trap 'teardown 143; exit 143' TERM

# ------------------------------------------------------------- utilities

# wait_or_die <seconds> <what it is waiting for> <command...>
# Every wait in this script goes through here, so none of them can be the
# one that hangs a cold machine forever, and every timeout says what it was
# waiting for rather than only that it waited.
wait_or_die() {
  local budget="$1" what="$2"
  shift 2
  local deadline=$(( $(date +%s) + budget ))
  until "$@" >/dev/null 2>&1; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      die "timed out after ${budget}s waiting for $what."
    fi
    sleep 1
  done
}

# ------------------------------------------- the engine control channel
#
# backupd#795. Only ever used with --break-engine, and every line of it is
# inert without that flag.
#
# engine_is_live is the same healthcheck the startup wait uses, asked of
# the engine's own listener from inside its container: "docker start
# returned" is not the same fact as "the engine is serving again", and a
# suite told the second when only the first is true fails on a race it
# cannot see.
engine_is_live() {
  docker exec "$c_engine" /backupd-web healthcheck --url http://127.0.0.1:8080/health/live >/dev/null 2>&1
}

# And the hop the BROWSER actually uses, which is not the same fact
# either. Found by running this: the smoke's recovery step failed on
# three 502s logged after "started" had been acknowledged.
#
# engine_is_live asks the engine's own listener from inside its own
# container. A browser reaches it through serve-ui's reverse proxy, in
# another container, over the internal network - and that path has its
# own state: Docker may hand the restarted container a different address,
# and serve-ui's http.Transport is holding idle keep-alive connections to
# the address the engine had BEFORE it went away. So there is a window in
# which the engine is serving, the ack has been written, and the first
# request the browser makes still comes back 502 from the proxy.
#
# This closes it by asking serve-ui to proxy the same healthcheck: the
# request is made INSIDE the web container against serve-ui's own port,
# and /health/ is one of the two prefixes it forwards (webhost/serve's
# NewUI), so a pass here means one real request has already traversed the
# whole hop and any stale connection has already been discarded on it
# rather than on a page the suite is about to assert against.
#
# It is the same principle as the acks themselves: what the reader
# depends on is what has to be verified, not the nearest thing that is
# cheap to check.
serve_ui_reaches_engine() {
  docker exec "$c_web" /backupd-web healthcheck --url http://127.0.0.1:8080/health/live >/dev/null 2>&1
}

# The health budget one "start" request is given, in seconds.
#
# 175 rather than the 120 this used to allow, because the number the
# suite waits is 180 (backupd-tests' startEngine) and a watcher that
# gives up at 120 reports a failure for an engine that would have been
# serving at 130 with a minute of the reader's patience left unspent.
# 175 and not 180 for the other end of the same arithmetic: the ack has
# to LAND inside the suite's budget to be read at all, so the few seconds
# this loop spends picking the request up and running `docker start` are
# left for the failure ack to be written in rather than raced against.
engine_start_health_budget=175

# The opposite fact from engine_is_live, and asked of Docker rather than
# of the engine: "docker stop returned 0" includes a container that was
# not there to stop and a daemon that accepted the request, so the state
# is read back before anything is acknowledged.
engine_is_stopped() {
  [ "$(docker inspect -f '{{.State.Running}}' "$c_engine" 2>/dev/null)" = "false" ]
}

# engine_ack <name> [reason]
#
# Written to a dotfile beside the ack and moved into place, so a reader
# polling for the name can never see half of a reason: rename within one
# directory is atomic, and the suite reads these files the instant they
# appear.
engine_ack() {
  local name="$1"
  # One line, and bounded. This string is quoted verbatim into the
  # exception the suite throws, and a page of docker output in a test
  # report is a page nobody reads to the end of.
  local reason
  reason="$(printf '%s' "${2:-}" | tr '\n\t' '  ' | cut -c1-200)"
  printf '%s\n' "$reason" > "$engine_control/.$name.tmp"
  mv -f "$engine_control/.$name.tmp" "$engine_control/$name"
}

# engine_watcher_loop is what the client container cannot do for itself.
# It has no Docker socket, deliberately: the thing under test is a browser
# on an edge network, and a browser that can stop containers is not one.
# So the capability is held here and exposed as a directory of files.
#
# Ordering inside each branch is the contract, not tidiness. The request
# file is removed BEFORE the work, so a second request can never be
# acknowledged by the first one's leftovers, and the ack is written AFTER
# it, so a suite that sees "started" can rely on the engine answering.
# `docker start` against a running container is a no-op that exits 0,
# which makes a redundant heal (a test that heals mid-run and again in a
# finally) acknowledge rather than fail.
#
# And an ack is only ever written for a state this watcher WATCHED the
# stack reach (#795's review, all four reviewers). Both branches used to
# `|| true` the docker command and write the success ack regardless, and
# the start branch wrote "started" even when the health loop had timed
# out. The suite across the repository boundary reads these files as
# proof: a discarded `docker stop` failure reads there as "the engine is
# unreachable" and runs the outage assertions against a healthy engine,
# and a timed-out start reads as "healthy" and runs the recovery
# assertions against an engine that never came back. Both then fail as
# product defects. So the exit status is kept, the state is read back,
# and a failure is acknowledged AS a failure: "stop-failed" /
# "start-failed", carrying a one-line reason the suite quotes. The
# contract is the same either way - exactly one file appears per request
# - which is what lets the suite stop inferring a rig fault from its own
# timeout.
engine_watcher_loop() {
  while :; do
    if [ -e "$engine_control/stop" ]; then
      rm -f "$engine_control/stop"
      local out=""
      if out="$(docker stop "$c_engine" 2>&1)" && engine_is_stopped; then
        rm -f "$engine_control/started" "$engine_control/stop-failed" "$engine_control/start-failed"
        engine_ack stopped
      else
        # Not "stopped", and not silence either. The engine is still up,
        # or Docker would not say, and the browser is about to be asked
        # to prove an outage that never happened.
        rm -f "$engine_control/stopped"
        engine_ack stop-failed "docker stop did not leave the engine stopped: ${out:-no output}"
      fi
    elif [ -e "$engine_control/start" ]; then
      rm -f "$engine_control/start"
      local out=""
      if ! out="$(docker start "$c_engine" 2>&1)"; then
        rm -f "$engine_control/started"
        engine_ack start-failed "docker start refused: ${out:-no output}"
      else
        # Bounded, and it gives up out loud rather than hanging or
        # lying. A container that is running but has not opened its
        # database yet answers the proxy with a refusal, so "started" is
        # withheld until the engine's own healthcheck passes AND the same
        # healthcheck passes through serve-ui's proxy, which is the hop
        # the browser uses and the one a restarted container's new
        # address invalidates. A budget that runs out is a start-failed
        # rather than a "started" the suite would read as a recovered
        # stack.
        local waited=0 live=0
        while [ "$waited" -lt "$engine_start_health_budget" ]; do
          if engine_is_live && serve_ui_reaches_engine; then live=1; break; fi
          sleep 1
          waited=$(( waited + 1 ))
        done
        if [ "$live" = 1 ]; then
          rm -f "$engine_control/stopped" "$engine_control/start-failed" "$engine_control/stop-failed"
          engine_ack started
        else
          rm -f "$engine_control/started"
          engine_ack start-failed "the engine container started but the browser's own path to it did not come good within ${engine_start_health_budget}s (its healthcheck, then the same check through serve-ui's proxy)"
        fi
      fi
    fi
    sleep 0.25
  done
}

start_engine_watcher() {
  mkdir -p "$engine_control"
  # The state the stack is actually in when the suite is handed it. The
  # suite is not expected to read it before asking for anything - it
  # removes the acks it is about to wait for first - but a directory whose
  # contents describe the world is easier to debug than an empty one.
  # The two failure acks are cleared for a sharper reason: one left behind
  # by an earlier run against the same artifacts directory would be read
  # by the first request of this one as its own answer.
  rm -f "$engine_control/stop" "$engine_control/start" "$engine_control/stopped" \
        "$engine_control/stop-failed" "$engine_control/start-failed"
  : > "$engine_control/started"
  engine_watcher_loop &
  engine_watcher_pid=$!
}

stop_engine_watcher() {
  [ -n "$engine_watcher_pid" ] || return 0
  kill "$engine_watcher_pid" >/dev/null 2>&1 || true
  wait "$engine_watcher_pid" 2>/dev/null || true
  engine_watcher_pid=""
}

# ----------------------------------- the workflow control channel (#816)
#
# The same shape as the engine channel above -- a directory of files,
# requests removed as they are picked up, exactly one ack per request,
# and a success ack only for a state this script watched the stack reach
# -- and the reasoning there is the reasoning here, so it is not
# restated. What differs is what the three verbs do.

workflow_ack() {  # workflow_ack <name> [reason]
  local name="$1"
  local reason
  reason="$(printf '%s' "${2:-}" | tr '\n\t' '  ' | cut -c1-200)"
  printf '%s\n' "$reason" > "$workflow_control/.$name.tmp"
  mv -f "$workflow_control/.$name.tmp" "$workflow_control/$name"
}

# The runner ANSWERING, which is not the same fact as its container
# running: `serve` proves a docker client, a reachable daemon, the hook
# image and bash inside it before it binds its socket (#865), and
# `status` is the same question the engine asks before it will validate a
# .local.sh hook. So this is the engine's own answer, asked the engine's
# own way.
runner_is_live() {
  docker exec "$c_runner" /backupd workflow-runner status \
    --runtime-dir /data/run \
    --workspace-dir "$wf_workspace" \
    --secrets-dir /data/secrets >/dev/null 2>&1
}

runner_is_stopped() {
  [ "$(docker inspect -f '{{.State.Running}}' "$c_runner" 2>/dev/null)" = "false" ]
}

# The runner's health budget for one "runner-up" request. Smaller than
# the engine's because what it waits for is smaller: a process that
# re-proves its container capability and binds a socket, with no database
# to open and no schema to migrate.
runner_start_health_budget=60

workflow_watcher_loop() {
  while :; do
    if [ -e "$workflow_control/crash" ]; then
      rm -f "$workflow_control/crash"
      local out=""
      # KILLED, not stopped, and that is the whole verb. `docker stop`
      # sends SIGTERM and the engine shuts a run down on its way out,
      # which produces a run that ENDED -- the opposite of the state
      # under test. SIGKILL leaves a run nobody observed the end of,
      # which is what startup reconciliation exists for and what a
      # cleanup obligation nobody settled looks like.
      if ! out="$(docker kill "$c_engine" 2>&1)"; then
        rm -f "$workflow_control/crashed"
        workflow_ack crash-failed "docker kill refused: ${out:-no output}"
      elif ! out="$(docker start "$c_engine" 2>&1)"; then
        rm -f "$workflow_control/crashed"
        workflow_ack crash-failed "the engine was killed and would not start again: ${out:-no output}"
      else
        local waited=0 live=0
        while [ "$waited" -lt "$engine_start_health_budget" ]; do
          if engine_is_live && serve_ui_reaches_engine; then live=1; break; fi
          sleep 1
          waited=$(( waited + 1 ))
        done
        if [ "$live" = 1 ]; then
          rm -f "$workflow_control/crash-failed"
          workflow_ack crashed
        else
          rm -f "$workflow_control/crashed"
          workflow_ack crash-failed "the engine was killed and restarted but the browser's own path to it did not come good within ${engine_start_health_budget}s"
        fi
      fi
    elif [ -e "$workflow_control/runner-down" ]; then
      rm -f "$workflow_control/runner-down"
      local out=""
      if out="$(docker stop "$c_runner" 2>&1)" && runner_is_stopped; then
        rm -f "$workflow_control/runner-started" "$workflow_control/runner-down-failed" "$workflow_control/runner-up-failed"
        workflow_ack runner-stopped
      else
        rm -f "$workflow_control/runner-stopped"
        workflow_ack runner-down-failed "docker stop did not leave the runner stopped: ${out:-no output}"
      fi
    elif [ -e "$workflow_control/runner-up" ]; then
      rm -f "$workflow_control/runner-up"
      local out=""
      if ! out="$(docker start "$c_runner" 2>&1)"; then
        rm -f "$workflow_control/runner-started"
        workflow_ack runner-up-failed "docker start refused: ${out:-no output}"
      else
        local waited=0 live=0
        while [ "$waited" -lt "$runner_start_health_budget" ]; do
          if runner_is_live; then live=1; break; fi
          sleep 1
          waited=$(( waited + 1 ))
        done
        if [ "$live" = 1 ]; then
          rm -f "$workflow_control/runner-stopped" "$workflow_control/runner-up-failed" "$workflow_control/runner-down-failed"
          workflow_ack runner-started
        else
          rm -f "$workflow_control/runner-started"
          workflow_ack runner-up-failed "the runner's container started but it did not answer its own status verb within ${runner_start_health_budget}s"
        fi
      fi
    fi
    sleep 0.25
  done
}

start_workflow_watcher() {
  mkdir -p "$workflow_control"
  rm -f "$workflow_control/crash" "$workflow_control/runner-down" "$workflow_control/runner-up" \
        "$workflow_control/crashed" "$workflow_control/crash-failed" \
        "$workflow_control/runner-stopped" "$workflow_control/runner-down-failed" \
        "$workflow_control/runner-up-failed"
  # The state the stack is actually in when the suite is handed it, for
  # start_engine_watcher's reason.
  : > "$workflow_control/runner-started"
  workflow_watcher_loop &
  workflow_watcher_pid=$!
}

stop_workflow_watcher() {
  [ -n "$workflow_watcher_pid" ] || return 0
  kill "$workflow_watcher_pid" >/dev/null 2>&1 || true
  wait "$workflow_watcher_pid" 2>/dev/null || true
  workflow_watcher_pid=""
}

# The VPS container, which is alpine and has coreutils. The product's
# containers do not, which is what the three functions below are for.
sha256_of() {  # sha256_of <container> <path>
  docker exec "$1" sha256sum "$2" | awk '{print $1}'
}

# toolbox runs a shell command in a throwaway container that has a shell,
# which is the VPS image this run already built. Everything the harness
# needs to ask about the DEPLOYMENT's own volumes goes through it, because
# the product's runtime image is distroless: no shell, no coreutils, nothing
# to ask with. `docker exec <engine> sha256sum` does not answer wrongly, it
# fails with "executable file not found", which is a confusing way to learn
# whether a backup landed.
#
# --entrypoint sh, and that part is load-bearing rather than tidy.
# atmoz/sftp's entrypoint generates a fresh pair of host keys and narrates
# it, randomart and all, on STDOUT before it runs the command it was given.
# Harmless when the container IS the VPS, and fatal when the container is
# being asked a question: a command substitution around it comes back with
# forty lines of ASCII art and the answer on the end.
toolbox() {  # toolbox <extra docker run args...> -- <shell command>
  local args=()
  while [ "${1:-}" != "--" ]; do args+=("$1"); shift; done
  shift
  docker run --rm --label "$label" --entrypoint sh "${args[@]}" "$source_image" -c "$1"
}

sha256_in_volume() {  # sha256_in_volume <volume> <path within it>
  toolbox -v "$1:/v:ro" -- "sha256sum '/v/$2' 2>/dev/null | awk '{print \$1}'"
}

exists_in_volume() {  # exists_in_volume <volume> <path within it>
  toolbox -v "$1:/v:ro" -- "test -f '/v/$2'"
}

list_volume() {  # list_volume <volume> <directory within it>
  toolbox -v "$1:/v:ro" -- "ls -1 '/v/$2' 2>/dev/null" | tr '\n' ' '
}

# A one-shot product container on the deployment's own volumes, for the
# three commands that run while nothing is serving: creating the backup set,
# enrolling the administrator, and the seeding cycle. The first two are
# REFUSED beside a running engine (#571, and the auth store's own
# process-lifetime flock) and the third loses a SQLite lock race with it,
# which is measured rather than assumed and is written up where it happens.
# All three are things an operator does before first start, so this is the
# honest order rather than a workaround.
oneshot() {  # oneshot <network, or "none"> <command...>
  local net="$1"; shift
  # The workflow mounts, and only when there are workflows: `backup-set
  # workflow patch` resolves the stage directory it is given and
  # `workflow env set --secret-file` resolves the file it names, so both
  # writes need the same view of them the engine will have. Expanded with
  # the +"${...}" form because an empty array under `set -u` is an error
  # on the bash this may run on, and a bare "${a[@]}" there would pass
  # docker an empty argument.
  local wf_mounts=()
  if [ "$workflows" = 1 ]; then
    wf_mounts=(-v "$v_workflows:/workflows:ro" -v "$v_wfsecrets:$engine_secrets_mount:ro")
  fi
  docker run --rm -i \
    --network "$net" \
    --label "$label" \
    --user "$app_uid:$app_gid" \
    -v "$v_config:/etc/backupd/config" \
    -v "$v_state:/data/state" \
    -v "$v_backups:/data/backups" \
    -v "$v_keys:/etc/backupd/keys:ro" \
    ${wf_mounts[@]+"${wf_mounts[@]}"} \
    -e TMPDIR=/tmp \
    "$product_image" "$@"
}

# ------------------------------------------------------------- preflight

step "preflight"
for tool in docker openssl; do
  command -v "$tool" >/dev/null 2>&1 \
    || cannot_run "$tool is not on PATH, and this stack needs it."
done
docker info >/dev/null 2>&1 \
  || cannot_run "the Docker daemon is not reachable." \
                "Start Docker and re-run. Reporting a pass for a run that never happened is the one thing this must not do."
note "docker daemon reachable"

if [ -n "$prebuilt_image" ]; then
  docker image inspect "$prebuilt_image" >/dev/null 2>&1 \
    || die "--image $prebuilt_image is not on this machine, and this script will not pull one." \
           "A published tag would test somebody else's build, which is #342. Build it, or drop --image."
fi

if [ "$workflows" = 1 ]; then
  # The runner runs every local hook in an ephemeral container, so the
  # machine it runs on needs a docker client and a daemon it can reach.
  # In this rig that machine is itself a container, so the SOCKET is what
  # has to be handed to it -- the sibling-container shape, and the reason
  # the runner's workspace is a host path mounted at the same path inside
  # it: the daemon resolves a hook's mounts on the host.
  #
  # Read from the active Docker context rather than assumed to be
  # /var/run/docker.sock, because on Docker Desktop it is not: it is a
  # socket under the user's own home, and a rig that mounted a path that
  # is not there would hand the runner a daemon it cannot reach and then
  # report the refusal as a product fault.
  docker_socket="${RM_DOCKER_SOCKET:-}"
  if [ -z "$docker_socket" ]; then
    docker_endpoint="$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null || true)"
    case "$docker_endpoint" in
      unix://*) docker_socket="${docker_endpoint#unix://}" ;;
      *) docker_socket="/var/run/docker.sock" ;;
    esac
  fi
  [ -S "$docker_socket" ] \
    || cannot_run "the Docker daemon is not reachable over a Unix socket ($docker_socket is not one)." \
                  "The Host Workflow Runner runs local hooks in containers, so it needs that socket; name it with RM_DOCKER_SOCKET, or re-run with --no-workflows and accept that every workflow case will skip."
  note "docker socket for the runner: $docker_socket"
fi

mkdir -p "$run_dir/upload" "$run_dir/authorized_keys" "$run_dir/hookdata" "$artifacts_dir"
# The client container runs as this host's own uid so the traces and
# screenshots it writes are owned by the person who has to read them, so
# this directory only has to be writable by that same uid.
chmod 700 "$run_dir" "$artifacts_dir"

# ---------------------------------------------------------- the images

if [ -z "$prebuilt_image" ]; then
  step "building the product image from this working tree"
  version="$(git rev-parse --short HEAD)"
  commit="$(git rev-parse HEAD)"
  if ! git diff --quiet HEAD 2>/dev/null || ! git diff --cached --quiet 2>/dev/null; then
    commit="$commit-dirty"
  fi
  note "VERSION=$version COMMIT=$commit"
  docker build \
    -f container/Dockerfile \
    --build-arg "VERSION=$version" \
    --build-arg "COMMIT=$commit" \
    -t "$product_image" \
    . \
    || die "could not build the product image from this working tree." \
           "Everything below tests that image, so there is nothing to fall back to."
  created_images+=("$product_image")
else
  note "using the product image already on this machine: $product_image"
fi

step "building the VPS and client machine images"
docker build -q -t "$source_image" -f "$source_dockerfile" "$(dirname "$source_dockerfile")" >/dev/null \
  || die "could not build the VPS machine image from $source_dockerfile."
# The client image's context is scripts/e2e, because web-ui-smoke.mjs is
# copied into it and the browser has to be able to find @playwright/test
# next to that file.
docker build -q -t "$client_image" -f "$client_dockerfile" "$(dirname "$client_dockerfile")" >/dev/null \
  || die "could not build the client machine image from $client_dockerfile." \
         "It is the pinned Playwright image plus the runner: if the pull failed, this machine has no route to mcr.microsoft.com."
note "VPS machine:    $source_image"
note "client machine: $client_image"
if [ "$front_proxy" = 1 ]; then
  docker build -q -t "$proxy_image" -f "$proxy_dockerfile" "$(dirname "$proxy_dockerfile")" >/dev/null \
    || die "could not build the front-proxy machine image from $proxy_dockerfile."
  created_images+=("$proxy_image")
  note "front proxy:    $proxy_image (TLS + HTTP/2, #730 reproduction)"
fi

if [ "$workflows" = 1 ]; then
  step "building the exec host and the workflow runner machine images"
  # The exec host is scripts/e2e/exec-host.Dockerfile, which
  # core/tests/machines already builds for the same purpose: one sshd
  # carrying accounts that differ ONLY in what the server will let them
  # run. This rig needs two of those accounts and one client key across
  # both, because #810's claim is that an SFTP transfer credential must
  # not be assumed to grant shell exec, and a claim about two
  # capabilities cannot be proven against one account.
  docker build -q -t "$exec_image" -f "$exec_dockerfile" "$(dirname "$exec_dockerfile")" >/dev/null \
    || die "could not build the exec host image from $exec_dockerfile."
  # The runner's machine is the product's own binary plus a docker
  # client, so PRODUCT_IMAGE is the image this run is testing: the
  # runner refuses an engine from a different release, and building the
  # runner from the same image is the only way to be sure of the match
  # rather than to hope for it.
  docker build -q -t "$runner_image" -f "$runner_dockerfile" \
    --build-arg "PRODUCT_IMAGE=$product_image" "$(dirname "$runner_dockerfile")" >/dev/null \
    || die "could not build the workflow runner machine image from $runner_dockerfile."
  created_images+=("$runner_image")
  note "exec host:      $exec_image"
  note "runner machine: $runner_image"
  # And the image local hooks run IN. `workflow-runner serve` proves this
  # image is present before it binds its socket and refuses rather than
  # pulling it, because a preflight that reached for the network would
  # hang on a NAS with no route out. So the pull is the rig's job.
  #
  # --platform, and the ARCHITECTURE IS CHECKED rather than the presence,
  # which is what this run learned the hard way: a plain `docker pull` of
  # this reference on an arm64 Docker Desktop came back with the amd64
  # image, and the runner then refused to serve at all --
  #
  #   the hook image ... is built for linux/amd64 and this host's daemon
  #   runs linux/arm64. Under emulation a hook is an order of magnitude
  #   slower and its libc is untested here, and the docker client prints
  #   a warning about it into every hook's stderr, which this runner
  #   streams to the operator as the hook's own output
  #
  # which is the right refusal and a confusing way to find out that a
  # cached image is the wrong one. Asking the daemon what it runs and the
  # image what it is makes a mismatch a re-pull rather than a mystery.
  hook_platform="$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}' 2>/dev/null || true)"
  [ -n "$hook_platform" ] || die "could not ask the Docker daemon which platform it runs."
  if [ "$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$hook_image" 2>/dev/null || true)" != "$hook_platform" ]; then
    docker pull -q --platform "$hook_platform" "$hook_image" >/dev/null \
      || cannot_run "the hook image $hook_image is not on this machine for $hook_platform and could not be pulled." \
                    "Every local hook runs in a container from it, so the runner would refuse to serve and every workflow case would be a skip."
  fi
  note "hook image:     $hook_image ($hook_platform)"
fi

# ------------------------------------------------------------- payload

step "seeding the VPS's files"
# Deterministic bytes rather than /dev/urandom, so a digest mismatch can be
# reasoned about rather than only observed.
head -c 3145728 /dev/zero \
  | openssl enc -aes-256-ctr -pbkdf2 -pass pass:backupd-e2e-web-ui -nosalt 2>/dev/null \
  > "$run_dir/upload/payload.bin" \
  || die "could not generate the payload."
printf 'CREATE TABLE artifacts (id text primary key);\n' > "$run_dir/upload/schema.sql"
printf 'the browser suite runs against this machine, not against a mock.\n' > "$run_dir/upload/notes.txt"
# The sshd container runs as its own uid and has to read these, and this
# directory is a per-run path under the system temp directory that teardown
# removes by name.
chmod 644 "$run_dir/upload"/*
chmod 755 "$run_dir/upload"
note "3 files: payload.bin (3 MiB), schema.sql, notes.txt"

if [ "$workflows" = 1 ]; then
  # What the twelve workflow sets pull, and it is deliberately tiny. The
  # three files above are the VPS's, and the digests of them are what the
  # real-path cases assert; these sets exist to run HOOKS, and every byte
  # they transfer is a byte the deployment's own first cycle has to move
  # before a browser can press anything. One file, under a kilobyte, and
  # it is still a real SFTP transfer of a real file with a real digest.
  printf 'one small file, so twelve workflow runs cost seconds rather than minutes.\n' \
    > "$run_dir/hookdata/marker.txt" \
    || die "could not write the workflow sets' payload."
  chmod 644 "$run_dir/hookdata/marker.txt"
  chmod 755 "$run_dir/hookdata"
  note "1 file for the workflow sets: marker.txt"
fi

# ------------------------------------------------------------- volumes

step "creating the deployment's volumes and its SSH key"
for v in "$v_keys" "$v_config" "$v_state" "$v_backups"; do
  docker volume create --label "$label" "$v" >/dev/null \
    || die "could not create the volume $v."
  created_volumes+=("$v")
done

# The keypair is generated INSIDE a container and stays in a volume this
# script destroys, so the private half never touches this host. The public
# half comes back out through stdout, because that is not a secret and the
# VPS has to be told to accept it.
#
# 0600 on the key and 0700 on the directory holding it are not hygiene
# theatre: core/internal/transport/rclone/ssh.go refuses anything else, by
# design, and refuses a group- or world-writable ancestor as well.
toolbox -v "$v_keys:/keys" -- "
    set -e
    ssh-keygen -q -t ed25519 -N '' -C 'backupd e2e' -f /keys/id_ed25519 </dev/null
    chown -R $app_uid:$app_gid /keys
    chmod 700 /keys
    chmod 600 /keys/id_ed25519
    chmod 644 /keys/id_ed25519.pub
  " >/dev/null \
  || die "could not generate the deployment's SSH keypair inside the key volume."

engine_pubkey="$(toolbox -v "$v_keys:/keys:ro" -- "cat /keys/id_ed25519.pub")"
[ -n "$engine_pubkey" ] || die "the generated keypair has no public half."
printf '%s\n' "$engine_pubkey" > "$run_dir/authorized_keys/engine.pub"
chmod 644 "$run_dir/authorized_keys/engine.pub"
note "keypair generated in the volume $v_keys, public half ${engine_pubkey:0:38}..."

# The config, state and backup volumes start out owned by root, and the
# product runs as $app_uid with no shell and no root step to fix that from
# the inside. Chowned here, once, with the machine image that is already
# local rather than a pull.
toolbox -v "$v_config:/c" -v "$v_state:/s" -v "$v_backups:/b" \
  -- "chown $app_uid:$app_gid /c /s /b && chmod 755 /c /s /b" >/dev/null \
  || die "could not give the deployment's volumes to $app_uid:$app_gid."
note "config, state and backup volumes belong to $app_uid:$app_gid"

if [ "$workflows" = 1 ]; then
  step "provisioning the workflow machinery (#816)"

  for v in "$v_workflows" "$v_wfrun" "$v_wfsecrets"; do
    docker volume create --label "$label" "$v" >/dev/null \
      || die "could not create the volume $v."
    created_volumes+=("$v")
  done

  # The script library. Committed under scripts/e2e/workflows/ rather
  # than written here by a here-document, for one reason: every line
  # those scripts print is asserted by a suite in another repository, so
  # they are a contract, and a contract belongs in a file a reviewer can
  # read and `bash -n` can parse.
  #
  # The modes are not hygiene theatre. core/internal/workflow/discover.go
  # refuses a stage directory this process does not own, one that is
  # group- or world-writable, and one with a writable ancestor, so the
  # tree is chowned to the uid the engine runs as and left at 0755/0644.
  # A Docker volume is the same filesystem the container reads, so a mode
  # set here is the mode the engine sees -- which a bind mount from this
  # host cannot promise across every Docker implementation.
  toolbox -v "$repo_root/scripts/e2e/workflows:/src:ro" -v "$v_workflows:/w" -- "
      set -e
      cp -R /src/. /w/
      chown -R $app_uid:$app_gid /w
      find /w -type d -exec chmod 755 {} +
      find /w -type f -exec chmod 644 {} +
    " >/dev/null \
    || die "could not seed the workflow script library into the volume $v_workflows."
  note "script library: $(list_volume "$v_workflows" ".")in $v_workflows"

  # The runner's two other directories. 0700 and owned by the account
  # the runner runs as, which is hostrunner.RuntimeDirMode's own rule and
  # is enforced by the runner at startup: it chmods what it can and
  # refuses what it cannot, and a root-owned volume is the second.
  toolbox -v "$v_wfrun:/r" -v "$v_wfsecrets:/s" \
    -- "chown $app_uid:$app_gid /r /s && chmod 700 /r /s" >/dev/null \
    || die "could not give the runner's runtime and secrets volumes to $app_uid:$app_gid."

  # The installation credential, and the one secret a hook of this run
  # will be handed. Both go down a PIPE into a container rather than onto
  # a command line: an argv is readable by every process on this host for
  # as long as the container lives, which is the exposure the
  # administrator's password already avoids the same way.
  #
  # 64 hex characters, which is what scripts/install/install_docker_host.py
  # writes and is well past hostrunner.MinTokenLength; mode 0600, which
  # LoadToken refuses anything looser than.
  runner_token="$(openssl rand -hex 32)"
  printf '%s\n' "$runner_token" | toolbox -i -v "$v_wfsecrets:/s" -- "
      set -e
      cat > /s/$runner_token_name
      chown $app_uid:$app_gid /s/$runner_token_name
      chmod 600 /s/$runner_token_name
    " >/dev/null \
    || die "could not write the workflow runner's installation credential."
  printf '%s\n' "$wf_secret" | toolbox -i -v "$v_wfsecrets:/s" -- "
      set -e
      cat > /s/$wf_secret_name
      chown $app_uid:$app_gid /s/$wf_secret_name
      chmod 600 /s/$wf_secret_name
    " >/dev/null \
    || die "could not write the hook environment's secret."
  note "the runner's credential and one hook secret are in $v_wfsecrets, mode 0600"

  # The runner's WORKSPACE, and the one path in this rig that has to be a
  # host directory rather than a volume. The runner asks the DAEMON to
  # bind-mount each step's working directory into that step's hook
  # container, and the daemon resolves those paths on the host -- so the
  # runner's own view of them has to be the host's view. It is mounted at
  # the same path inside the runner's container for exactly that reason.
  #
  # Its parent is 0755 and not 0700: the runner runs as the deployment's
  # uid rather than as this host's user, and a 0700 ancestor owned by
  # somebody else is a directory it cannot traverse. The workspace itself
  # is 0700 and owned by that uid, which is what holds whatever a hook
  # writes into BACKUPD_WORK_DIR -- and created from inside a container
  # so the ownership is real on a Linux host rather than only mapped.
  mkdir -p "$wf_prefix" || die "could not create $wf_prefix."
  chmod 755 "$wf_prefix"
  toolbox -v "$wf_prefix:/p" -- "
      set -e
      mkdir -p /p/workspace
      chown $app_uid:$app_gid /p/workspace
      chmod 700 /p/workspace
    " >/dev/null \
    || die "could not create the runner's workspace at $wf_workspace."
  note "runner workspace: $wf_workspace, 0700 and owned by $app_uid:$app_gid"
fi

# ------------------------------------------------------------ networks

step "creating the three private networks"
for n in "$net_edge" "$net_internal" "$net_backhaul"; do
  docker network create --label "$label" "$n" >/dev/null \
    || die "could not create the network $n."
  created_networks+=("$n")
done
note "edge     $net_edge      (client <-> the product's front door)"
note "internal $net_internal  (serve-ui <-> the engine)"
note "backhaul $net_backhaul  (the engine <-> the VPS)"

# ---------------------------------------------------------- the VPS

step "starting the VPS"
# No host keys are mounted in: atmoz/sftp generates its own on first start,
# and `backup-set create --trust-host-key` below pins whatever it presents.
# two-machine-backup.sh mounts a fixed pair because core/tests/sftpfixture
# wants a known_hosts line settled in advance; nothing here does, and a
# keypair not generated is a keypair not left on this host.
docker run -d \
  --name "$c_source" \
  --network "$net_backhaul" \
  --network-alias vps \
  --label "$label" \
  -v "$run_dir/authorized_keys:/home/$sftp_user/.ssh/keys:ro" \
  -v "$run_dir/upload:/home/$sftp_user/upload" \
  "$source_image" "$sftp_user::$sftp_uid:$sftp_uid:upload" >/dev/null \
  || die "could not start the VPS."
created_containers+=("$c_source")

wait_or_die 120 "the VPS's sshd to start listening" \
  docker exec "$c_source" sh -c 'nc -w 2 127.0.0.1 22 </dev/null 2>/dev/null | grep -q ^SSH-'
note "$c_source is serving SSH on the backhaul network as \"vps\""

# The digests every assertion below is against, read from the VPS itself
# rather than from the copy this script wrote. What has to match is the
# bytes the machine being backed up is actually serving.
want_payload="$(sha256_of "$c_source" "/home/$sftp_user/upload/payload.bin")"
want_schema="$(sha256_of "$c_source" "/home/$sftp_user/upload/schema.sql")"
want_notes="$(sha256_of "$c_source" "/home/$sftp_user/upload/notes.txt")"
note "payload.bin on the VPS is sha256 $want_payload"

if [ "$workflows" = 1 ]; then
  step "starting the exec host, the machine a remote hook really runs on"
  # atmoz's entrypoint is gone from that image (it creates chrooted SFTP
  # users and execs its own sshd, and this image is an sshd of its own),
  # which also means nothing generates host keys. `ssh-keygen -A` makes
  # the missing ones INSIDE the container, so the fixture's private host
  # keys never touch this host -- the same rule the deployment's own
  # keypair follows, and the reason the VPS mounts none either.
  docker run -d \
    --name "$c_exec" \
    --network "$net_backhaul" \
    --network-alias exechost \
    # The hostname the remote hooks PRINT, and it has to be a name
    # rather than Docker's default short container id: the suite
    # reads that line as evidence that a hook ran on the far side,
    # and evidence that changes every run is evidence nobody can
    # pin. Same string as the network alias, which is the name
    # everything else in this rig calls this machine.
    --hostname exechost \
    --label "$label" \
    -v "$run_dir/authorized_keys/engine.pub:/etc/ssh/authorized/backupd.pub:ro" \
    -v "$run_dir/hookdata:/home/$exec_user/hookdata" \
    --entrypoint sh \
    "$exec_image" -c 'ssh-keygen -A >/dev/null && exec /usr/sbin/sshd -D -e -f /etc/ssh/sshd_config.exec' >/dev/null \
    || die "could not start the exec host."
  created_containers+=("$c_exec")

  wait_or_die 120 "the exec host's sshd to start listening" \
    docker exec "$c_exec" sh -c 'nc -w 2 127.0.0.1 22 </dev/null 2>/dev/null | grep -q ^SSH-'
  note "$c_exec is serving SSH on the backhaul network as \"exechost\""

  # And the host keys it is presenting, pinned. A declared execution
  # connection has no --trust-host-key to fall back on: core/internal/config
  # refuses an sftp remote with no known_hosts and refuses the literal
  # "none", by design, so the pin has to be a real file holding the keys
  # this machine actually answers with.
  #
  # Read out of the container rather than scanned over the network, and
  # for a plain reason: ssh-keyscan is in openssh-client and the machine
  # images here carry openssh-SERVER, so a scan would mean pulling a
  # package into a throwaway container on every run (#243 is what that
  # costs). The public halves are files in the container that generated
  # them, `docker exec cat` is enough to read them, and what is pinned is
  # then exactly what sshd loaded rather than what a scan happened to
  # negotiate.
  #
  # Both key types, for the reason core/tests/machines states: x/crypto/ssh
  # picks a host-key algorithm by its own preference order rather than by
  # what known_hosts holds, so pinning one type is not pinning the
  # connection.
  known_hosts=""
  for keytype in ed25519 rsa; do
    pub="$(docker exec "$c_exec" cat "/etc/ssh/ssh_host_${keytype}_key.pub" 2>/dev/null || true)"
    case "$pub" in
      ssh-*)
        # The comment field goes: a known_hosts line is host, algorithm
        # and key, and the trailing "root@<container id>" is neither.
        known_hosts="$known_hosts
exechost $(printf '%s' "$pub" | cut -d' ' -f1,2)" ;;
    esac
  done
  [ -n "$known_hosts" ] \
    || die "the exec host presented no host keys this script could read." \
           "Its sshd generates them on start (ssh-keygen -A), so an empty answer here means it never got that far."
  # Down a pipe into the volume, owned by the engine's uid because the
  # engine is what reads it, and 0600 because core's SSH transport
  # refuses a credential path anything can write.
  printf '%s\n' "$known_hosts" | toolbox -i -v "$v_keys:/keys" -- "
      set -e
      sed '/^$/d' > /keys/known_hosts
      test -s /keys/known_hosts
      chown $app_uid:$app_gid /keys/known_hosts
      chmod 600 /keys/known_hosts
    " >/dev/null \
    || die "could not write the exec host's pinned host keys into $v_keys."
  note "the exec host's host keys are pinned in $v_keys/known_hosts"
fi

# ------------------------------------------- configure the deployment

step "creating the backup set, before anything is serving"
# --trust-host-key probes the VPS now and trusts what answers, which is why
# the VPS is up first. The alternative, --known-hosts-line, would need a
# key this script had settled in advance, and it settles none.
oneshot "$net_backhaul" \
  /backupd backup-set create "$backup_set" \
    --config /etc/backupd/config \
    --host vps \
    --user "$sftp_user" \
    --ssh-key-file /etc/backupd/keys/id_ed25519 \
    --trust-host-key \
    --remote-path /upload \
    --local-path /data/backups/vps \
    --completion-strategy rename \
    --read-only \
    --state-database /data/state/state.db \
  || die "creating the backup set failed." \
         "This ran with nothing serving, on the deployment's own volumes, so a refusal here is about the arguments or the VPS, not about #571."
note "backup set $backup_set points at the VPS over SSH, read-only"

step "enrolling the administrator"
# Down a pipe into stdin, never on a command line and never in a file:
# `auth create-admin --password-stdin` is the product's own way to make an
# administrator without a browser, and it is what the deployment's first-run
# flow would otherwise do interactively.
printf '%s' "$admin_pass" | oneshot none \
  /backupd-web auth create-admin --username "$admin_user" --password-stdin \
  || die "could not enrol the administrator."
note "administrator $admin_user enrolled, password generated this run"

step "running one backup cycle, so the pages have something real to render"
# BEFORE the engine starts, and this order is load-bearing rather than
# stylistic. `backupd run` beside a serving engine is two processes writing the
# same SQLite journal, and the loser gets SQLITE_BUSY: measured here, a
# cycle run that way came back with
#
#   lifecycle: transfer: recording TRANSFERRED: state: update artifact:
#   database is locked (5) (SQLITE_BUSY)
#   e2e/vps backed nothing up this cycle: 2 walked, 0 got through
#
# on the second attempt, having got clean through on the first. Nothing
# about the deployment needs the engine up for this: seeding is setup, and
# the other two setup steps already run against a stopped deployment for
# the same family of reason (#571, and the auth store's own flock). So this
# joins them rather than racing them.
#
# It is also the proof that the engine can reach the VPS over SSH, and a
# stronger one than a banner grab: it authenticates with the generated key,
# lists a directory, pulls three files and verifies them.
oneshot "$net_backhaul" \
  /backupd run --config /etc/backupd/config \
  || die "the backup cycle exited non-zero, so the deployment could not pull from the VPS." \
         "Everything the browser is about to look at would be empty, and a suite passing against empty tables proves nothing."

# Optionally run more cycles to grow the durable activity journal past the
# size a three-file seed produces. #730's throw is on the AUTHENTICATED
# /api/v1/activity payload, and a larger one is likelier to cross whatever
# streaming/framing threshold a plain seed never reaches. Default 1 leaves
# the base rig byte-identical; the verify below still holds because the
# files on the VPS do not change between cycles.
seed_cycles="${RM_SEED_CYCLES:-1}"
if [ "$seed_cycles" -gt 1 ]; then
  step "running $((seed_cycles - 1)) more backup cycle(s) to enlarge the activity journal"
  i=1
  while [ "$i" -lt "$seed_cycles" ]; do
    oneshot "$net_backhaul" /backupd run --config /etc/backupd/config \
      || die "seed cycle $((i + 1)) of $seed_cycles exited non-zero."
    i=$((i + 1))
  done
  note "$seed_cycles cycles run; the activity feed holds more than the first three events"
fi

for pair in "payload.bin:$want_payload" "schema.sql:$want_schema" "notes.txt:$want_notes"; do
  name="${pair%%:*}"
  want="${pair##*:}"
  exists_in_volume "$v_backups" "vps/$name" \
    || die "$name never landed in the deployment's backup volume." \
           "The volume holds: $(list_volume "$v_backups" vps)"
  got="$(sha256_in_volume "$v_backups" "vps/$name")"
  [ "$got" = "$want" ] \
    || die "$name landed with different bytes from the VPS's." "VPS:  $want" "here: $got" \
           "A file that exists is not a backup, and a page rendering a row about it would be rendering a lie."
done
note "three artifacts landed and every one matches the VPS by sha256"

if [ "$workflows" = 1 ]; then
  step "creating the workflow backup sets, one per scenario"
  # Every one of them is a REAL backup set over a REAL SSH connection to
  # a real directory, and that is what makes the scenarios mean
  # anything: "the before hook failed, so the backup was skipped" is
  # only a proof if the backup it skipped would otherwise have
  # succeeded. Each gets its own local path so no two write over each
  # other, and none is scheduled beyond the deployment's own poll.
  #
  # They pull from the EXEC HOST rather than from the VPS, and from a
  # directory holding one small file rather than three files and three
  # megabytes. Two reasons, both learned from running this:
  #
  #   The machine a hook quiesces is the machine being backed up. That
  #   is what a workflow IS for, and here it makes the set's transfer
  #   credential and its execution connection the same account on the
  #   same host, which is the shape an operator would actually have.
  #
  #   And the deployment's own first cycle runs every set. With twelve
  #   sets pulling three megabytes each, that cycle was four minutes of
  #   the rig's wall clock, and a suite pressing "Run this backup set"
  #   during it waited behind a queue it could not see. One small file
  #   makes each of these runs seconds rather than tens of seconds --
  #   and the hooks, not the transfer, are what these sets exist for.
  #
  # e2e/vps keeps the three-file payload and the digest assertions: it
  # is the set the real-path cases are about.
  for spec in "${wf_sets[@]}"; do
    name="${spec%%:*}"
    dir="${spec##*:}"
    oneshot "$net_backhaul" \
      /backupd backup-set create "$name" \
        --config /etc/backupd/config \
        --host exechost \
        --user "$exec_user" \
        --ssh-key-file /etc/backupd/keys/id_ed25519 \
        --trust-host-key \
        --remote-path "/home/$exec_user/hookdata" \
        --local-path "/data/backups/$dir" \
        --completion-strategy rename \
        --read-only \
        --state-database /data/state/state.db >/dev/null \
      || die "creating the backup set $name failed." \
             "The workflow scenarios each need a set of their own, so there is nothing to fall back to."
  done
  note "${#wf_sets[@]} workflow backup sets created, each pulling from $exec_user@exechost:/home/$exec_user/hookdata, read-only"
fi


if [ "$workflows" = 1 ]; then
  step "configuring the workflows, before anything is serving"
  # AFTER the seeding cycle above, and that order is the fixture rather
  # than convenience. e2e/vps is about to be given a NAME.remote.sh hook
  # over an internal-sftp-forced credential, and the run preflight
  # (core/service/workflowpreflight.go) refuses a run whose hooks cannot
  # execute BEFORE it transfers anything -- so a set configured first
  # would have no artifacts at all and the SFTP-only case would be
  # proving nothing about transfer. The cycle ran while the set had no
  # hooks, which is the honest sequence: a deployment that backed this up
  # yesterday and configured a hook today.
  #
  # The block is written AS A BLOCK, with a here-document, and the reason
  # is that two of the four things in it have no CLI at all. `settings
  # workflow patch` writes the root and the global stages; the runner's
  # socket and credential are deliberately not settable that way (the two
  # ends of that socket see different paths, which config/workflows.go
  # argues at length), and neither is a declared execution connection. So
  # this is what an operator writes, and every CLI write below
  # re-marshals the whole document rather than editing it in place, which
  # is what makes a hand-written field survive them.
  #
  # The paths are the CONTAINER's, because that is what the field means:
  # the engine reads its token at the path IT sees, which is the whole of
  # backupd#877's lesson -- an in-container token_file naming the
  # installer's host path is a file that does not exist and every
  # .local.sh hook fails authentication. container/compose.yaml binds
  # that credential as a single file at /etc/backupd/workflow-runner.token;
  # here it arrives in the secrets volume the runner also reads, which is
  # the one deviation, and it is a deviation about Docker volumes versus
  # bind-mounted files rather than about the contract: a volume is the
  # only shape whose 0600-owned-by-1000 means that on every Docker
  # implementation this rig runs on.
  toolbox -i -v "$v_config:/c" -- "cat >> /c/config.yaml" <<YAML >/dev/null || die "could not write the workflows block into config.yaml."
workflows:
  root: /workflows
  runner:
    socket: /data/run/$runner_socket_name
    token_file: $engine_token_path
  exec_connections:
    - id: $exec_connection
      remote:
        type: sftp
        host: exechost
        port: 22
        user: $exec_user
        key_file: /etc/backupd/keys/id_ed25519
        known_hosts: /etc/backupd/keys/known_hosts
YAML
  note "workflows.root=/workflows, the runner's socket and credential, and the \"$exec_connection\" execution connection"

  # And each set's own stage directories, through the CLI, which is what
  # an operator has for this half. `backup-set workflow patch` is refused
  # beside a serving engine with the file untouched, so it belongs here
  # with the other pre-start writes rather than later.
  for spec in "${wf_stages[@]}"; do
    IFS='|' read -r name before after conn <<<"$spec"
    args=(/backupd backup-set workflow patch "$name" --config /etc/backupd/config)
    [ -z "$before" ] || args+=(--before-dir "$before")
    [ -z "$after" ] || args+=(--after-dir "$after")
    [ -z "$conn" ] || args+=(--exec-connection "$conn")
    oneshot none "${args[@]}" >/dev/null \
      || die "configuring the hooks of $name failed." \
             "Its stage directories are ${before:-none} and ${after:-none} under /workflows, seeded above."
  done
  note "${#wf_stages[@]} sets have hook stages; $wf_set_none has none, which is the state the empty surface renders"

  # The secret-backed variable, on the one set whose hook reads it. A
  # LOCATION and never material: the CLI takes no value for this and
  # config.yaml holds the path rather than the secret, which is the same
  # rule the repository passphrase follows.
  oneshot none /backupd backup-set workflow env "$wf_set_secret" set "$wf_secret_env" \
    --config /etc/backupd/config \
    --secret-file "$engine_secret_path" >/dev/null \
    || die "could not configure $wf_secret_env on $wf_set_secret."
  note "$wf_secret_env on $wf_set_secret reads $engine_secret_path, which only the engine can see"

  # Read back what was written, through the product's own resolver rather
  # than by grepping the file this script just appended to. What matters
  # is not that the text landed but that the CLI writes above did not
  # drop it on their way through: a `backup-set workflow patch` that
  # re-marshalled the document without the runner block would leave a
  # deployment that cannot run a local hook, and every workflow case
  # would then fail as a product defect.
  resolved="$(oneshot none /backupd settings workflow --config /etc/backupd/config 2>&1 || true)"
  case "$resolved" in
    *"/data/run/$runner_socket_name"*) : ;;
    *) die "the deployment does not report the host runner after its configuration was written." \
           "\`settings workflow\` said: $(printf '%s' "$resolved" | tr '\n' ' ' | cut -c1-400)" ;;
  esac
  case "$resolved" in
    *"$exec_connection"*) : ;;
    *) die "the deployment does not report the \"$exec_connection\" execution connection." \
           "A remote hook would then run over the backup set's own SFTP-only credential, which is the other case entirely." ;;
  esac
  note "the deployment reports both the runner and the execution connection"

  # And the cadence, which is the one thing that decides whether a
  # browser can watch a workflow run happen at all.
  #
  # It cannot start one. POST /api/v1/operations run_backup_set is
  # refused 403 DESTRUCTIVE_OPERATIONS_DISABLED on every deployment this
  # repository can build: apps/common/webhost ships exactly one
  # DestructiveGate, NotYetImplementedGate, whose own doc says there is
  # deliberately no parameter, variable or flag that can make it report
  # true before #92. So the "Run this backup set" control cannot produce
  # a run, and a suite that presses it waits out its timeout on a
  # request the engine was never going to be allowed to accept.
  #
  # What DOES produce runs is the scheduler, and gate.go says so in as
  # many words: "the scheduler runs the same destructive cycle on a
  # timer whatever this reports". So the rig sets that timer to the
  # product's own floor -- config.MinPollInterval, one minute, and a
  # shorter value is refused rather than accepted -- and hands the
  # number to the suite as RM_WF_POLL_SECONDS so no spec has to guess
  # it. Each workflow set then gets a fresh run every poll interval plus
  # its position in the cycle, which is what a spec waits for instead of
  # asking.
  #
  # Edited in place rather than patched through the CLI because there is
  # no `settings patch --poll-interval` in this build, and appended-block
  # style would not work for a key `backup-set create` has already
  # written: the line is replaced, and the replacement is verified by
  # reading the file back rather than by trusting sed's exit status.
  toolbox -v "$v_config:/c" -- "
      set -e
      sed -i 's/^poll_interval:.*/poll_interval: ${wf_poll_seconds}s/' /c/config.yaml
      grep -q '^poll_interval: ${wf_poll_seconds}s\$' /c/config.yaml
    " >/dev/null \
    || die "could not set the deployment's poll_interval to ${wf_poll_seconds}s." \
           "Without it the engine polls at its own default and a browser would wait an hour for the run it is meant to watch."
  note "poll_interval is ${wf_poll_seconds}s (config.MinPollInterval, the product's floor), so every set runs on a timer a browser can wait for"
fi

# ------------------------------------------- the Host Workflow Runner

if [ "$workflows" = 1 ]; then
  step "starting the Host Workflow Runner, OUTSIDE the engine's container"
  # This is the process the engine cannot be. `/backupd-web serve` is
  # distroless, read-only, capability-dropped and non-root on purpose, so
  # "run this operator's shell script on the host" is a thing it
  # deliberately cannot do; docs/adr/0020-host-workflow-runner.md is the
  # decision and this container is its shape in the rig. It holds the
  # Docker socket -- which is the privilege the installer grants the
  # runner's service account and withholds from the engine -- and the
  # engine reaches it through one authenticated Unix socket and nothing
  # else.
  #
  # The socket's GROUP is read back rather than assumed: inside a
  # container it is 0 on Docker Desktop and the docker group's gid on a
  # Linux host, and the runner refuses to run as root, so the membership
  # is how a non-root process opens it. Exactly the `usermod -aG docker`
  # the installer performs, expressed as --group-add.
  docker_socket_gid="$(toolbox -v "$docker_socket:/var/run/docker.sock" \
    -- 'stat -c %g /var/run/docker.sock' 2>/dev/null | tr -d '[:space:]')"
  case "$docker_socket_gid" in
    ''|*[!0-9]*) die "could not read the group of the Docker socket $docker_socket from inside a container." \
                     "The runner runs as $app_uid:$app_gid and would be refused by the daemon without that group." ;;
  esac

  # --network none, because it needs none: it talks to the daemon over
  # the socket and to the engine over its own. The config volume is
  # mounted read-only and named as --config for the same reason the
  # installer's unit does: the runner reads the script size bound from
  # it, and nothing else.
  docker run -d \
    --name "$c_runner" \
    --network none \
    --label "$label" \
    --user "$app_uid:$app_gid" \
    --group-add "$docker_socket_gid" \
    -e TMPDIR=/tmp \
    -v "$docker_socket:/var/run/docker.sock" \
    -v "$v_wfrun:/data/run" \
    -v "$v_wfsecrets:/data/secrets" \
    -v "$v_config:/etc/backupd/config:ro" \
    -v "$wf_workspace:$wf_workspace" \
    "$runner_image" /backupd workflow-runner serve \
      --runtime-dir /data/run \
      --workspace-dir "$wf_workspace" \
      --secrets-dir /data/secrets \
      --config /etc/backupd/config \
      --docker /usr/local/bin/docker \
      --hook-image "$hook_image" \
      --hook-bash /usr/local/bin/bash >/dev/null \
    || die "could not start the Host Workflow Runner."
  created_containers+=("$c_runner")

  # Readiness is the runner ANSWERING, not the container running: `serve`
  # proves a docker client, a reachable daemon, the hook image and bash
  # inside it before it binds anything (#865), so a container that is up
  # and a runner that is serving are different facts and only the second
  # is one the engine can use.
  # In a SUBSHELL, because wait_or_die dies: it calls die, die exits, and
  # an `if !` around it could never run its else branch. That made this
  # diagnostic dead code on the rig's hardest failure mode -- a runner
  # that refuses to serve says WHY on its own stdout (no docker client, no
  # daemon, the hook image absent, the wrong platform), and that sentence
  # was being thrown away. Run in a subshell the exit only leaves, so the
  # logs are printed here and the refusal below is this script's.
  if ! ( wait_or_die 120 "the workflow runner to answer its own status verb" runner_is_live ); then
    echo "    the runner's own last words:" >&2
    docker logs "$c_runner" 2>&1 | tail -20 >&2 || true
    die "the Host Workflow Runner never came up." \
        "Its startup proves a docker client, a reachable daemon, the hook image and bash inside it before it binds a socket (#865), and it prints which of those it could not do."
  fi
  note "$(docker logs "$c_runner" 2>&1 | sed -n '1,2p' | tr '\n' ' ')"
  note "$c_runner is serving on the socket in $v_wfrun, as $app_uid:$app_gid with group $docker_socket_gid"
fi

# --------------------------------------------------------- the engine

step "starting the engine (/backupd-web serve)"
# The environment is container/compose.yaml's own for this service, and the
# values that differ from it differ for a reason written beside them.
# The workflow mounts are container/compose.yaml's own three, and the
# differences between them are the whole security argument it makes: the
# scripts READ-ONLY because this container reads each one once and
# spools it, the runtime directory read-write because connecting to a
# Unix socket is a write, and the credential because without it the
# other two buy nothing. No capability, no privilege, no Docker socket
# and no host root are added here: the engine stays exactly as hardened
# as it was, and everything the hooks need lives on the other side of
# that socket.
engine_run=(
  docker run -d
  --name "$c_engine"
  --network "$net_internal"
  --network-alias engine
  --label "$label"
  --user "$app_uid:$app_gid"
  -e TMPDIR=/tmp
  -e LISTEN_ADDR=":8080"
  -e PUBLIC_BASE_URL="$base_url"
  -e TRUST_FORWARDED_HEADERS="true"
  -v "$v_config:/etc/backupd/config"
  -v "$v_state:/data/state"
  -v "$v_backups:/data/backups"
  -v "$v_keys:/etc/backupd/keys:ro"
)
if [ "$workflows" = 1 ]; then
  engine_run+=(
    -v "$v_workflows:/workflows:ro"
    -v "$v_wfrun:/data/run"
    -v "$v_wfsecrets:$engine_secrets_mount:ro"
  )
fi
engine_run+=("$product_image" /backupd-web serve --profile=generic)
"${engine_run[@]}" >/dev/null \
  || die "could not start the engine."
created_containers+=("$c_engine")

# The engine also needs the backhaul network, and it is attached after the
# start rather than at it because `docker run` takes one --network. The
# alias is the name the backup set was created against.
docker network connect --alias manager "$net_backhaul" "$c_engine" \
  || die "could not put the engine on the backhaul network, so it has no route to the VPS."

wait_or_die 180 "the engine to report itself live" \
  docker exec "$c_engine" /backupd-web healthcheck --url http://127.0.0.1:8080/health/live
note "$c_engine is serving on the internal network as \"engine\", and is on the backhaul network as \"manager\""

if [ "$workflows" = 1 ]; then
  # The engine polls on start, and that first cycle is every set in the
  # deployment, hooks and all. It must be OVER before a browser is handed
  # the stack, and this is the lesson of a whole red run: a suite that
  # pressed "Run this backup set" while it was in flight watched its
  # sixty seconds go by with no new run appearing, because the engine was
  # working through eleven other sets first -- including a hook that
  # deliberately holds for forty-five seconds. Nothing was broken. The
  # request was in a queue nobody could see, and the failure read as
  # "the engine recorded nothing".
  #
  # The wait is on the engine's OWN structured event rather than on a
  # sleep or on an API poll: `cycle_end` is what the product writes when
  # a cycle is finished, and one of them is exactly the fact this needs.
  # It also means every set has a first run and a settled state before
  # the suite looks, which is a better deployment to hand over than a
  # half-cycled one.
  cycle_finished() {
    docker logs "$c_engine" 2>&1 | grep -q '"event":"cycle_end"'
  }
  wait_or_die 600 "the engine's first poll cycle to finish, so a browser's run request is not queued behind it" \
    cycle_finished
  note "the engine's first cycle is done: every set has run once, hooks included"
fi

if [ "$workflows" = 1 ]; then
  step "proving the engine can reach the runner, and that an SFTP-only credential cannot exec"
  # `validate workflow` is the product's own answer to "could this set's
  # hooks run?", asked from INSIDE the engine's container, which is the
  # only place the question means anything: the socket and the credential
  # are paths as THAT process sees them, and backupd#877 was exactly the
  # case where both were configured, both existed on the host, and
  # neither was there from in here.
  #
  # Every report is kept under the artifacts directory as well as
  # asserted, because these three are the evidence that the fixture IS
  # the fixture -- a suite case that later fails over an exec-capability
  # finding is read completely differently depending on whether this
  # deployment's own tooling agreed with it.
  #
  # validation_of <set> <file> prints one report and files it.
  validation_of() {
    local set_id="$1" name="$2" out=""
    out="$(docker exec "$c_engine" /backupd validate workflow "$set_id" --config /etc/backupd/config 2>&1 || true)"
    printf '%s\n' "$out" > "$artifacts_dir/validate-$name.txt"
    printf '%s' "$out"
  }

  # says <report> <extended regex>
  says() { printf '%s' "$1" | grep -Eq "$2"; }

  # first_line_about <report> <check id>, for a refusal that has to quote
  # what the product said rather than only that it disagreed.
  first_line_about() {
    printf '%s' "$1" | grep -E "^ +$2 " | head -1 | sed -e 's/^ *//' -e 's/  */ /g' | cut -c1-300
  }

  happy_validation="$(validation_of "$wf_set_happy" happy)"
  says "$happy_validation" 'runner_health +ok' \
    || die "the engine does not report a healthy Host Workflow Runner for $wf_set_happy." \
           "The runner is serving on this host and the socket is mounted into the engine, so this is the backupd#877 shape: $(first_line_about "$happy_validation" runner_health)"
  says "$happy_validation" 'the host workflow runner answered' \
    || die "the runner's health check passed without the runner having answered, which is not a fact this rig can use."
  says "$happy_validation" 'local_bash_syntax +ok' \
    || die "the runner would not parse $wf_set_happy's local hooks." \
           "$(first_line_about "$happy_validation" local_bash_syntax)"
  says "$happy_validation" 'workflow valid: +true' \
    || die "$wf_set_happy's hooks are not valid, so a browser would meet a deployment that cannot run one." \
           "Every finding is in $artifacts_dir/validate-happy.txt"
  note "$wf_set_happy validates: the engine reached the runner over the mounted socket, and the runner parsed both hooks"

  # And the other half of #810, which is a REFUSAL and has to be proven
  # as one. The same client key authenticates both accounts; the
  # difference is what the server will let each run, so a refusal here is
  # the sshd's own and not this rig's.
  sftp_validation="$(validation_of "$backup_set" sftp-only)"
  says "$sftp_validation" 'exec_capability +error' \
    || die "$backup_set's remote hook was not refused for want of exec capability." \
           "That set's credential is internal-sftp-forced, so a validation that passes means the fixture is not the fixture: $(first_line_about "$sftp_validation" exec_capability)"
  says "$sftp_validation" 'valid for backup: +true' \
    || die "$backup_set reports itself invalid for backup, and it is not: its artifacts are in the catalogue already." \
           "The SFTP-only case is \"the transfer works and the hook is refused\", and half of it has just stopped being true."
  note "$backup_set is refused an exec channel and is still valid for backup: $(first_line_about "$sftp_validation" exec_capability)"

  # The exec-capable one, which must pass where that one failed.
  exec_validation="$(validation_of "$wf_set_remote" exec)"
  says "$exec_validation" 'exec_capability +ok' \
    || die "$wf_set_remote cannot run a remote hook either, so the rig has no exec-capable posture at all." \
           "$(first_line_about "$exec_validation" exec_capability)"
  says "$exec_validation" 'workflow valid: +true' \
    || die "$wf_set_remote's hooks are not valid, so no remote hook in this rig has anywhere to run." \
           "Every finding is in $artifacts_dir/validate-exec.txt"
  note "$wf_set_remote validates over the \"$exec_connection\" connection: $(first_line_about "$exec_validation" exec_capability)"
fi

# --------------------------------------------------------- the UI host

step "starting the UI host (/backupd-web serve-ui)"
docker run -d \
  --name "$c_web" \
  --network "$net_internal" \
  --network-alias web-ui \
  --label "$label" \
  --user "$app_uid:$app_gid" \
  --read-only \
  --tmpfs "/tmp:size=16m,mode=1777" \
  -e TMPDIR=/tmp \
  -e LISTEN_ADDR=":8080" \
  -e UPSTREAM_ADDR="http://engine:8080" \
  "$product_image" /backupd-web serve-ui --profile=generic >/dev/null \
  || die "could not start the UI host."
created_containers+=("$c_web")

# And the edge network, where it answers to the name the browser is given.
# This is the ONLY container on that network besides the client, so a spec
# that tried to reach the engine directly would find nothing at all, which
# is the point of there being three networks rather than one.
docker network connect --alias "$edge_web_alias" "$net_edge" "$c_web" \
  || die "could not put the UI host on the edge network, so the client would have nothing to talk to."

wait_or_die 180 "the UI host to answer its own listener" \
  docker exec "$c_web" /backupd-web healthcheck
note "$c_web is serving on the edge network as \"$edge_web_alias\", proxying to \"engine\""

if [ "$front_proxy" = 1 ]; then
  step "starting the TLS + HTTP/2 front proxy (backupd#730 reproduction)"
  # An ordinary reverse proxy in front of serve-ui, taking the edge-network
  # name the client is given and upstreaming to serve-ui's "origin" alias.
  # This is the hop a real NAS has and the plain-HTTP rig did not: the
  # browser now negotiates HTTP/2 over TLS instead of HTTP/1.1 in the clear,
  # which is the transport the client request is identical to every other
  # page's on yet #730 says only /api/v1/activity fails over.
  docker run -d \
    --name "$c_proxy" \
    --network "$net_edge" \
    --network-alias backupd \
    --label "$label" \
    "$proxy_image" >/dev/null \
    || die "could not start the front proxy."
  created_containers+=("$c_proxy")

  proxy_up=0
  for _ in $(seq 1 30); do
    if toolbox --network "$net_edge" -- 'nc -z -w 3 backupd 443'; then
      proxy_up=1; break
    fi
    sleep 1
  done
  [ "$proxy_up" = 1 ] \
    || die "the front proxy never accepted TLS on 443 on the edge network."
  note "$c_proxy terminates TLS + HTTP/2 as \"backupd\", upstream to \"$edge_web_alias\""
fi

# ============================================ the two reachability proofs

step "proving the wire, both hops"

# The engine's own network position, borrowed. --network container: shares
# that container's namespace and its resolver, so this is the route the
# engine has and not a route that resembles it. The engine's image is
# distroless and has no ssh client of its own to ask with.
ssh_banner="$(toolbox --network "container:$c_engine" -- \
  'nc -w 5 vps 22 </dev/null 2>/dev/null | head -1' || true)"
case "$ssh_banner" in
  SSH-*) note "from the engine's network namespace, vps:22 answers \"$ssh_banner\"" ;;
  *) die "from the engine's network namespace, vps:22 did not answer with an SSH banner (got: ${ssh_banner:-nothing})." ;;
esac

# And from the client's, the page the browser is about to open. Run in a
# throwaway on the edge network from the client image itself, so what is
# proven reachable is reachable from the thing that will do the reaching.
login_probe="$(docker run --rm --label "$label" --network "$net_edge" \
  -e "RM_BASE_URL=$base_url" $front_proxy_probe_env "$client_image" \
  node -e '
    (async () => {
      const r = await fetch(process.env.RM_BASE_URL + "/");
      const body = await r.text();
      const title = (body.match(/<title[^>]*>([^<]*)<\/title>/i) || [, ""])[1];
      console.log(r.status + " " + (r.headers.get("content-type") || "") + " " + body.length + " bytes, <title>" + title + "</title>");
    })().catch((e) => { console.log("no answer: " + e.message); process.exitCode = 1; });
  ' 2>&1 || true)"
case "$login_probe" in
  "200 text/html"*) note "from the client's network, GET $base_url/ answers $login_probe" ;;
  *) die "the client could not fetch the Web UI from $base_url/ (got: ${login_probe:-nothing})." \
         "This is the hop the whole stack exists for, so nothing below is worth running until it works." ;;
esac

# ========================================= the break, rehearsed (#795)

if [ "$break_engine" = 1 ]; then
  step "--break-engine: rehearsing the fault before handing the stack over"

  # api_probe <what it is for> -> "<status> <correlation id or ->"
  #
  # /api/v1/activity unauthenticated, from the edge network, which is the
  # exact route and the exact position the browser will fail from. The
  # request carries no session, so a working stack refuses it 401 FROM
  # THE ENGINE; a stack whose engine is gone is answered 502 by serve-ui
  # itself. Those two numbers are the whole proof, and telling them apart
  # is why this probe asks for an API route rather than for the bundle:
  # the bundle is served by serve-ui either way and says nothing about
  # the hop behind it.
  api_probe() {
    docker run --rm --label "$label" --network "$net_edge" \
      -e "RM_BASE_URL=$base_url" $front_proxy_probe_env "$client_image" \
      node -e '
        (async () => {
          const r = await fetch(process.env.RM_BASE_URL + "/api/v1/activity");
          console.log(r.status + " " + (r.headers.get("x-correlation-id") || "-"));
        })().catch((e) => { console.log("no-answer " + e.message); process.exitCode = 1; });
      ' 2>&1 || true
  }

  healthy_probe="$(api_probe)"
  # 401 or 403: both are the ENGINE having looked at an unauthenticated
  # request and refused it, which is the only fact this half needs. The
  # break below is the strict one (502, and an id on it), because that is
  # the assertion the mode exists to make; pinning the healthy side to one
  # exact status would let a gate variation nobody is testing here stop
  # the run.
  case "$healthy_probe" in
    401\ *|403\ *) note "with the engine up, GET /api/v1/activity answers $healthy_probe" ;;
    *) die "with the engine up, GET /api/v1/activity answered \"${healthy_probe:-nothing}\", want a 401 or 403 from the engine." \
           "The break below is only meaningful against a stack that was working, so this refuses to rehearse on one that was not." ;;
  esac

  docker stop "$c_engine" >/dev/null || die "could not stop the engine for the rehearsal."
  broken_probe="$(api_probe)"
  case "$broken_probe" in
    502\ -) die "with the engine stopped, GET /api/v1/activity answered 502 with no X-Correlation-Id." \
                "The browser's banner would then have nothing to quote and the proxy_error line in serve-ui's log nothing to be joined by (#795)." ;;
    502\ *) note "with the engine stopped, GET /api/v1/activity answers $broken_probe, from serve-ui rather than the engine" ;;
    *) docker start "$c_engine" >/dev/null 2>&1 || true
       die "with the engine stopped, GET /api/v1/activity answered \"${broken_probe:-nothing}\", want 502 from serve-ui." \
           "Either serve-ui went down with the engine, in which case this mode is testing something else entirely, or something is answering for it." ;;
  esac

  docker start "$c_engine" >/dev/null || die "could not start the engine again after the rehearsal."
  wait_or_die 180 "the engine to come back up after the rehearsal" engine_is_live
  healed_probe="$(api_probe)"
  case "$healed_probe" in
    401\ *|403\ *) note "and with it back, $healed_probe again: the break is reversible, which the recovery half of the suite needs" ;;
    *) die "after restarting the engine, GET /api/v1/activity answered \"${healed_probe:-nothing}\", want the engine's own refusal again." \
           "A break that cannot be undone leaves no way to assert that the pages recover." ;;
  esac

  start_engine_watcher
  note "the control channel is live at $engine_control (write \"stop\" or \"start\", wait for \"stopped\" or \"started\")"
fi

if [ "$workflows" = 1 ] && [ "$keep_up" != 1 ]; then
  start_workflow_watcher
  note "the workflow control channel is live at $workflow_control (crash, runner-down, runner-up)"
fi

# ------------------------------------------------------- hand it over

step "the stack is up"
note "RM_BASE_URL       $base_url"
note "RM_ADMIN_USERNAME $admin_user"
note "RM_ADMIN_PASSWORD $admin_pass"
note "RM_BACKUP_SET     $backup_set"
note "RM_ARTIFACTS_DIR  /artifacts, mounted from $artifacts_dir"
if [ "$workflows" = 1 ]; then
  note "RM_WORKFLOW_SET   $wf_set_happy"
  note "RM_EXEC_SET       $wf_set_remote"
  note "RM_SFTP_ONLY_SET  $backup_set"
  note "RM_WF_*_SET       $wf_set_none, $wf_set_before_fail, $wf_set_after_fail, $wf_set_hostile,"
  note "                  $wf_set_secret, $wf_set_slow, $wf_set_crash, $wf_set_many,"
  note "                  $wf_set_findings"
  note "RM_WF_SCRIPT_PREFIX /workflows"
  note "RM_WF_POLL_SECONDS $wf_poll_seconds (the scheduler's cadence: a browser cannot start a run, #92)"
else
  note "workflows        NOT provisioned (--no-workflows), so every workflow case skips itself"
fi

client_env=(
  -e "RM_BASE_URL=$base_url"
  -e "RM_ADMIN_USERNAME=$admin_user"
  -e "RM_ADMIN_PASSWORD=$admin_pass"
  -e "RM_BACKUP_SET=$backup_set"
  -e "RM_ARTIFACTS_DIR=/artifacts"
  -e "RM_CHROMIUM_NO_SANDBOX=${RM_CHROMIUM_NO_SANDBOX:-0}"
  -e "HOME=/tmp"
)
# Over the self-signed front proxy the browser and any node fetch in the
# suite must accept the leaf; RM_IGNORE_HTTPS switches on Playwright's
# ignoreHTTPSErrors in web-ui-smoke.mjs, and NODE_TLS_REJECT_UNAUTHORIZED
# covers a suite that fetches from node. Appended after the array literal so
# an empty case never expands to a stray argument.
if [ "$front_proxy" = 1 ]; then
  client_env+=(-e "RM_IGNORE_HTTPS=1" -e "NODE_TLS_REJECT_UNAUTHORIZED=0" -e "NODE_NO_WARNINGS=1")
fi
# #816's contract. Unset without the machinery, so a spec that reads
# RM_WORKFLOW_SET gets undefined and says why it skipped rather than
# failing against a deployment that has no hooks at all.
if [ "$workflows" = 1 ]; then
  client_env+=(
    -e "RM_WORKFLOW_SET=$wf_set_happy"
    -e "RM_EXEC_SET=$wf_set_remote"
    -e "RM_SFTP_ONLY_SET=$backup_set"
    -e "RM_WF_NO_HOOKS_SET=$wf_set_none"
    -e "RM_WF_BEFORE_FAIL_SET=$wf_set_before_fail"
    -e "RM_WF_AFTER_FAIL_SET=$wf_set_after_fail"
    -e "RM_WF_HOSTILE_SET=$wf_set_hostile"
    -e "RM_WF_SECRET_SET=$wf_set_secret"
    -e "RM_WF_SLOW_SET=$wf_set_slow"
    -e "RM_WF_CRASH_SET=$wf_set_crash"
    -e "RM_WF_MANY_STEPS_SET=$wf_set_many"
    -e "RM_WF_FINDINGS_SET=$wf_set_findings"
    -e "RM_WF_REJECTED_DIR=$wf_rejected_dir"
    -e "RM_WF_SECRET_ENV=$wf_secret_env"
    -e "RM_WF_SECRET_VALUE=$wf_secret"
    -e "RM_WF_SCRIPT_PREFIX=/workflows"
    -e "RM_WF_POLL_SECONDS=$wf_poll_seconds"
    -e "RM_WF_GLOBAL_BEFORE_DIR=global-before"
    -e "RM_WORKFLOW_RUNNER_NAME=$runner_display"
  )
  # The control channel goes the same way --break-engine's does, and is
  # left off under --keep-up for the same reason: the watcher that acks
  # these requests is a process of this script, and a suite holding a
  # channel nobody answers would wait out its whole timeout and report
  # this rig's silence as a product failure.
  if [ "$keep_up" != 1 ]; then
    client_env+=(-e "RM_WORKFLOW_CONTROL=$workflow_control_in_client")
    note "RM_WORKFLOW_CONTROL $workflow_control_in_client, watched on this host at $workflow_control"
  fi
fi
# backupd#795. The flag the suite branches on, and the directory it drives
# the break from. Same appended-after-the-literal shape as the block
# above, and for the same reason: off, neither variable exists at all, so
# a spec that reads RM_ENGINE_UNREACHABLE gets undefined and asserts the
# healthy feed.
#
# And NOT under --keep-up, which is the other half of that same rule
# (#795's review). The watcher that acks these requests is a background
# process of this script, and the --keep-up exit tears it down, so a
# command carrying these two variables would hand a suite a control
# channel with nobody on the other end: every engine-unreachable case
# would sit out its full timeout and then report a rig failure as a
# product one. Left off, RM_ENGINE_UNREACHABLE is simply undefined in
# that command and those cases skip themselves, which is the honest
# answer. The block printed with the command says so and gives the
# by-hand equivalent.
if [ "$break_engine" = 1 ] && [ "$keep_up" != 1 ]; then
  client_env+=(-e "RM_ENGINE_UNREACHABLE=1" -e "RM_ENGINE_CONTROL=$engine_control_in_client")
  note "RM_ENGINE_UNREACHABLE 1"
  note "RM_ENGINE_CONTROL     $engine_control_in_client, watched on this host at $engine_control"
fi

client_run=(
  docker run
  --name "$c_client"
  --network "$net_edge"
  --label "$label"
  # Chromium's renderers put their shared buffers in /dev/shm, and Docker's
  # 64 MiB default is not enough for a 1440x900 page: the browser dies
  # mid-run with a page crash and no useful message. --shm-size rather than
  # --ipc=host, because sharing the host's IPC namespace to work around a
  # size default is a much larger hole than the size default is a problem.
  --shm-size=1g
  # Chromium leaves zombies when its parent is pid 1 and pid 1 is a test
  # runner rather than an init. A run that ends with a few hundred defunct
  # processes still exits, and then the next one has no pids left.
  --init
  # The invoking user's uid, so the traces and screenshots below land on
  # the host owned by whoever has to read them. The image's /suite is
  # a+rwX for exactly this: the uid has no account in that image.
  --user "$(id -u):$(id -g)"
  "${client_env[@]}"
  -v "$artifacts_dir:/artifacts"
)

if [ -n "$suite_dir" ]; then
  # The suite is MOUNTED rather than copied into the image, and the choice
  # is about which of the two changes more often. The specs change every
  # time somebody works on them and the runtime changes when a lockfile
  # moves, so mounting keeps the image stable across a day of spec edits
  # and keeps the rebuild for the thing that actually needs one. It costs
  # self-containment: this image alone does not carry a suite, and a run on
  # a machine without the tests checkout has nothing to mount.
  #
  # The anonymous volume over node_modules is what makes it work. Docker
  # seeds a volume from the image's own content at that path, so /suite is
  # the checkout and /suite/node_modules is the image's linux install, and
  # the checkout's darwin-built one is never in the picture.
  client_run+=(-v "$suite_dir:/suite" -v "/suite/node_modules" -w /suite)
  client_run+=("$client_image" npx playwright test)
  [ "$keep_up" = 1 ] || note "running the suite at $suite_dir inside the client container"
else
  client_run+=(-w /suite "$client_image" node smoke.mjs)
  [ "$keep_up" = 1 ] || note "running the built-in browser check (pass --suite DIR to run a Playwright suite instead)"
fi

if [ "$keep_up" = 1 ]; then
  step "--keep-up, so nothing is run and nothing is torn down"
  echo ""
  echo "    Drive it with:"
  echo ""
  printf '        '
  sep=""
  for word in "${client_run[@]}"; do
    printf '%s' "$sep"
    printf '%q' "$word"
    sep=" "
  done
  printf '\n'
  echo ""
  echo "    The teardown line is printed below. Nothing here is published to a host port,"
  echo "    so the only way to reach the UI is from a container on $net_edge."
  if [ "$break_engine" = 1 ]; then
    echo ""
    echo "    --break-engine's watcher is NOT left running: it holds this host's Docker socket,"
    echo "    and a loop outliving the script that started it is a loop nobody owns. So"
    echo "    RM_ENGINE_UNREACHABLE and RM_ENGINE_CONTROL are deliberately NOT in the command"
    echo "    above: with them set, every engine-unreachable case would write a request into a"
    echo "    directory nobody is watching, wait out its whole timeout, and report the rig's"
    echo "    silence as a product failure. Without them those cases skip themselves and the"
    echo "    rest of the suite runs against a healthy stack."
    echo ""
    echo "    Break the engine by hand, which is all the watcher does:"
    echo ""
    echo "        docker stop $c_engine     # serve-ui stays up and answers 502"
    echo "        docker start $c_engine    # and the pages recover"
    echo ""
    echo "    To drive the engine-unreachable cases against this stack, either re-run without"
    echo "    --keep-up, or export the two variables into your own suite run and ack the"
    echo "    requests yourself, in the watcher's own order (request file first, then the ack):"
    echo ""
    echo "        RM_ENGINE_UNREACHABLE=1 RM_ENGINE_CONTROL=$engine_control_in_client"
    echo "        rm -f $engine_control/stop  && docker stop  $c_engine && : > $engine_control/stopped"
    echo "        rm -f $engine_control/start && docker start $c_engine && : > $engine_control/started"
  fi
  if [ "$workflows" = 1 ]; then
    echo ""
    echo "    The workflow machinery IS up: the runner is $c_runner, the exec host is $c_exec,"
    echo "    and the seeded script library is in the volume $v_workflows. RM_WORKFLOW_CONTROL is"
    echo "    left off the command above for the watcher's reason, so the crash and runner-down"
    echo "    cases skip themselves. By hand, which is all that watcher does:"
    echo ""
    echo "        docker kill  $c_engine && docker start $c_engine   # a crash, then the reconcile"
    echo "        docker stop  $c_runner                             # the runner goes away"
    echo "        docker start $c_runner                             # and comes back"
  fi
  exit 0
fi

step "the client machine drives the browser"
created_containers+=("$c_client")
client_status=0
"${client_run[@]}" || client_status=$?

echo ""
if [ "$client_status" = 0 ]; then
  echo "==> three-machine: PASSED. A real browser signed in to a real deployment and read real data."
else
  echo "==> three-machine: FAILED. The client container exited $client_status." >&2
  echo "    Its evidence is at $artifacts_dir, and the teardown never removes that." >&2
fi
echo "    artifacts: $artifacts_dir"

# The client's status, deliberately, and not the teardown's. The EXIT trap
# runs teardown and then passes this through untouched, so a run that tore
# down cleanly after a red suite is still a red run.
exit "$client_status"
