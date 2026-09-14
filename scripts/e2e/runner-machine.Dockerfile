# The Host Workflow Runner's machine (#816), and the one definition of it.
#
# EPIC L splits hook execution in two, and the split is the whole security
# argument: the ENGINE is distroless, read-only, capability-dropped and
# non-root, so "run this operator's shell script on the host" is a thing it
# deliberately cannot do. `backupd workflow-runner serve` is the process
# that can, it runs OUTSIDE that container, and the only thing it exposes
# is one authenticated Unix socket (docs/adr/0020-host-workflow-runner.md).
#
# So the rig needs a machine for it, and this is the honest shape of one:
#
#   * The PRODUCT'S OWN BINARY, copied out of the image this run built.
#     Not a second build and not a host binary: the runner refuses an
#     engine from a different release, so version-matching is not a
#     detail, and the only way to be sure the two halves match is for
#     both to come from the same image. `/backupd` is CGO_ENABLED=0
#     (container/Dockerfile says so where it sets it), so the static
#     binary from a distroless image runs unchanged on this alpine base.
#
#   * A DOCKER CLIENT, because #865 made every local hook run in an
#     ephemeral container and `workflow-runner serve` proves that
#     capability before it binds its socket: a client it can execute and
#     a daemon it can reach, or it refuses to serve. docker:29-cli is
#     that client and nothing else.
#
# What this is NOT is the engine's posture. This machine holds the Docker
# socket, which is root-equivalent on the host, and that is exactly the
# privilege the real deployment grants the runner's service account (the
# installer's `usermod -aG docker`) and withholds from the engine
# container. A rig that gave the socket to the engine instead would be
# testing a deployment nobody ships.
#
# The runner still refuses to run as root (hostrunner.RefuseRoot), so this
# image declares no USER and the rig runs it as the deployment's own
# uid:gid with --group-add for the socket's group, which is the same two
# facts the systemd unit states as User= plus that group membership.
ARG PRODUCT_IMAGE
FROM ${PRODUCT_IMAGE} AS product

FROM docker:29-cli

COPY --from=product /backupd /backupd

# docker:29-cli's entrypoint is a wrapper that would prefix whatever this
# container is asked to run. The rig asks for `workflow-runner serve` with
# a long argument list of its own, so the prefix goes: the same reasoning
# container/Dockerfile gives for shipping no ENTRYPOINT at all.
ENTRYPOINT []
CMD ["/backupd", "version"]
