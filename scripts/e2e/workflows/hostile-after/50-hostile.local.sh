#!/usr/bin/env bash
# scripts/e2e/three-machine-web-ui.sh seeds this tree into the deployment's
# read-only /workflows mount. Every line of output here is asserted by
# backupdproject/backupd-tests' suites/web-ui, so the strings are a
# contract rather than narration (#816).
set -euo pipefail

# Output that tries to drive the terminal rather than write to it. A step
# log is attacker-influenced data -- an operator's hook echoes a filename,
# a database error or a remote banner -- so the viewer has to render these
# as text and do nothing at all with them.
#
# Three families, and each one is a real capability of a real terminal:
#
#   OSC 8  an embedded hyperlink. Rendered as navigation it is a link to
#          somewhere the operator did not choose, one click from the
#          session cookie that is already loaded.
#   OSC 0  set the window title. Rendered, the document title becomes
#          whatever the hook said, so the page lies about where it is.
#   CSI t  window manipulation (report/resize). Rendered, a log line moves
#          or measures the operator's window.
#
# Written with printf and explicit escapes rather than with a here-document
# holding literal control bytes, so the fixture stays greppable and a
# reviewer can see exactly which sequences are claimed.
printf '\033]8;;http://hostile.invalid/stolen\033\\CLICK ME\033]8;;\033\\\n'
printf '\033]0;e2e-hostile-title\007\n'
printf '\033[8;40;100t\n'
printf '\033[22t\n'
echo "e2e-hook hostile output done"
