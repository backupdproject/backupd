# The simulated host that hook scripts RUN on (#810), and the one
# definition of it.
#
# scripts/e2e/source-machine.Dockerfile is the VPS being backed up: atmoz's
# chrooted, internal-sftp-forced sshd, which is the posture docs/ssh-setup.md
# recommends for a backup SOURCE and which deliberately cannot run a shell
# command at all. That is exactly why a second machine definition has to
# exist: #810's whole claim is that an SFTP transfer credential must not be
# assumed to grant shell exec, and a claim about two different capabilities
# cannot be proven against one account.
#
# So this image runs its OWN sshd, from its own configuration, with three
# accounts that differ only in what the server will let them execute:
#
#   hookuser   an ordinary shell account: `ssh hookuser@host <cmd>` runs
#              <cmd>. This is the exec-capable execution connection.
#   sftponly   ForceCommand internal-sftp, exactly the atmoz/sftp posture:
#              authenticates, transfers files, and silently answers the
#              SFTP subsystem no matter what command the client asked for.
#   forcedcmd  ForceCommand a fixed wrapper, the rrsync/backup-shell shape:
#              authenticates, runs somebody else's program, and never the
#              client's.
#   contaminated
#              an ordinary shell account whose SERVER sets BASH_ENV for it.
#              bash reads BASH_ENV at startup, before any line this product
#              sends can unset it, so this is the one contamination
#              --noprofile --norc cannot defend against and the preflight
#              has to detect instead.
#
# All three authenticate with the SAME client key, on purpose. A test that
# gave them different keys could not tell "this credential may not exec"
# apart from "this credential does not authenticate", and the first of those
# is the fact #810 is about.
#
# Built on atmoz/sftp:alpine rather than on a bare alpine so that a daemon
# already running the machine tier needs no second registry pull: that base
# is the one every source machine already requires, and #243 is the record
# of what asking the registry for one more thing costs a gate.
FROM atmoz/sftp:alpine

# bash, because #810's envelope invokes a FIXED bash path with deterministic
# non-interactive options and the preflight refuses a host that has no bash
# rather than substituting ash. An image whose only shell were ash could not
# tell the refusal and the happy path apart.
RUN apk add --no-cache bash

# The three accounts. Shells differ because the server's own decision differs:
# sftponly gets nologin (sshd handles internal-sftp in-process and never
# spawns a shell for it), the other two get bash.
#
# The sed is not cosmetic. busybox `adduser -D` leaves the password field as
# "!", and sshd refuses a locked account outright ("User x not allowed
# because account is locked") before it ever looks at a key -- which reads
# exactly like "this key is not authorized" and would make the fixture prove
# the wrong thing. "*" is "no password login is possible", which is what a
# key-only account actually wants.
RUN adduser -D -s /bin/bash hookuser \
 && adduser -D -s /sbin/nologin sftponly \
 && adduser -D -s /bin/bash forcedcmd \
 && adduser -D -s /bin/bash contaminated \
 && sed -i -E 's/^(hookuser|sftponly|forcedcmd|contaminated):!/\1:*/' /etc/shadow

# The forced command, the rrsync shape: it ignores whatever the client asked
# for, says so, and exits successfully. Successfully is the point -- a
# forced-command account that failed loudly would be caught by any client;
# one that succeeds while running something else entirely is what a
# capability preflight has to catch.
RUN printf '%s\n' \
      '#!/bin/sh' \
      'echo "forced-command-only: this account runs $0 and nothing the client asked for"' \
      'exit 0' \
    > /usr/local/bin/forced-command \
 && chmod 0755 /usr/local/bin/forced-command

# This fixture's own sshd configuration, deliberately not atmoz's.
#
# StrictModes no is the one concession to being a fixture: the authorized
# keys arrive on a bind mount whose ownership is whatever the host's Docker
# implementation presents, and sshd's ownership checks on that path would
# make the fixture fail for a reason that has nothing to do with the code
# under test. Nothing else is relaxed: password and keyboard-interactive
# authentication are off, root cannot log in, and PermitUserEnvironment is
# off so the SERVER cannot inject environment either -- which matters here,
# because #810's envelope claims the environment a hook sees is the one this
# product built.
RUN printf '%s\n' \
      'Port 22' \
      'HostKey /etc/ssh/ssh_host_ed25519_key' \
      'HostKey /etc/ssh/ssh_host_rsa_key' \
      'PermitRootLogin no' \
      'PerSourcePenalties no' \
      'PasswordAuthentication no' \
      'KbdInteractiveAuthentication no' \
      'PubkeyAuthentication yes' \
      'AuthorizedKeysFile /etc/ssh/authorized/backupd.pub' \
      'StrictModes no' \
      'UsePAM no' \
      'PermitUserEnvironment no' \
      'AllowTcpForwarding no' \
      'X11Forwarding no' \
      'PrintMotd no' \
      'LogLevel VERBOSE' \
      'Subsystem sftp internal-sftp' \
      'Match User sftponly' \
      '  ForceCommand internal-sftp' \
      'Match User forcedcmd' \
      '  ForceCommand /usr/local/bin/forced-command' \
      'Match User contaminated' \
      '  SetEnv BASH_ENV=/usr/local/lib/contamination.sh' \
    > /etc/ssh/sshd_config.exec

# What the contaminated account's BASH_ENV points at. It is a real file so
# that the account is a working one whose only difference is the redirection:
# a preflight that refused it because the path was missing would be proving
# the wrong thing.
RUN printf '%s\n' 'export CONTAMINATED=yes' > /usr/local/lib/contamination.sh \
 && chmod 0644 /usr/local/lib/contamination.sh

# atmoz's entrypoint creates chrooted SFTP users and then execs its own
# sshd. Neither is wanted here, so both go: this image is an sshd and
# nothing else.
ENTRYPOINT []
CMD ["/usr/sbin/sshd", "-D", "-e", "-f", "/etc/ssh/sshd_config.exec"]
