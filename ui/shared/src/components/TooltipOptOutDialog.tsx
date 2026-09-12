/**
 * Issue #829: the question asked once, the first time an operator closes
 * a tooltip with its own "x".
 *
 * Closing one pop-up is an ambiguous act. It can mean "I have read this"
 * or "stop showing me these", and the difference is a preference this app
 * cannot guess. So it is asked, exactly once (state/tooltipNodes.ts owns
 * the "asked" flag and why it is separate), and the question is phrased
 * as the global change it makes rather than as the pop-up that was just
 * closed: answering Yes turns hover help off everywhere, which is a
 * larger thing than the "x" that opened this.
 *
 * Both answers are safe, so this is not a destructive dialog and the
 * confirm button is the ordinary primary one. What the copy must do
 * instead is say where the decision can be undone, because an operator
 * who turns the help off and later wants it back has no tooltip left to
 * click: hence the Settings link, which is also the acceptance criterion
 * #829 names. The link navigates and closes at once — a modal left up
 * over the page it just sent you to is a dialog that has to be dismissed
 * before the thing it pointed at can be used.
 *
 * Mounted once by App.tsx rather than once per tooltip host. A dialog per
 * pop-up would mean one modal per field on the page, all reading the same
 * node, and the pop-up that raised the question is closed by the time the
 * question is answered.
 */
import { Link } from "react-router-dom";
import { ConfirmationDialog } from "@shared/components/ConfirmationDialog";
import { useCausl } from "@shared/state/graph";
import {
  closeTooltipOptOutPrompt,
  setTooltipsEnabled,
  tooltipOptOutPromptNode
} from "@shared/state/tooltipNodes";

export function TooltipOptOutDialog() {
  const open = useCausl(tooltipOptOutPromptNode);

  return (
    <ConfirmationDialog
      open={open}
      eyebrow="Tooltips"
      title="Turn off tooltips?"
      confirmLabel="Yes, turn tooltips off"
      cancelLabel="Keep showing tooltips"
      onConfirm={() => {
        setTooltipsEnabled(false);
        closeTooltipOptOutPrompt();
      }}
      onCancel={closeTooltipOptOutPrompt}
    >
      <p style={{ margin: 0 }}>
        You closed a tooltip. Backupd can stop showing them on hover
        anywhere in this interface, on this browser.
      </p>
      <p style={{ margin: 0, color: "var(--text-2)" }}>
        This question is only asked once. Tooltips can be switched back on at any
        time under{" "}
        <Link to="/settings" onClick={closeTooltipOptOutPrompt}>
          Settings
        </Link>
        .
      </p>
    </ConfirmationDialog>
  );
}
