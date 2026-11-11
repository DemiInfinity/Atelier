// Small decisions of the "This device" screen, kept apart so they can be
// tested without a window.

/**
 * The workflow's name out of a waiting row's title. The title reads
 * `weekly-tidy wants to run for the first time`; the question in the
 * approval dialog only wants `weekly-tidy`. When the wording isn't one we
 * know, the whole title is used.
 */
export function workflowName(title: string): string {
  for (const mark of [" wants to ", " was started by ", " needs your approval"]) {
    const at = title.indexOf(mark);
    if (at > 0) return title.slice(0, at);
  }
  return title;
}

/** `detail. limits.` as the approval dialog's body, without doubled full stops. */
export function approvalBody(detail: string, limits: string): string {
  return [detail, limits]
    .map((part) => part.trim().replace(/\.+$/, ""))
    .filter((part) => part !== "")
    .map((part) => `${part}.`)
    .join(" ");
}

/**
 * Whether a transcript should follow new lines: only while the person is
 * (nearly) at its end. Scrolling up to read holds it still.
 */
export function atBottom(scrollTop: number, clientHeight: number, scrollHeight: number, slack = 12): boolean {
  return scrollHeight - scrollTop - clientHeight <= slack;
}
