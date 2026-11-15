/**
 * How long a waiver runs, as a field with three answers rather than a number.
 *
 * The dialog used to hold `days` as a `number` and write
 * `setDays(Number(e.target.value))`. `Number("")` is 0, the server reads a
 * non-positive `days` as *use the install's cap*, and the toast then said the
 * advisory was waived "for 0 days" — so clearing the box granted the longest
 * waiver this install allows while the screen said the shortest. A cleared
 * number field is an error state, never a nought.
 *
 * The draft is therefore a string, and this decides what it means. The cap is
 * the install's own (`waiver_max_days` on the dependency state), so the refusal
 * names the same number the server would.
 */
export interface WaiverDaysReading {
  /** The value to send, when there is one. */
  days?: number;
  /** The sentence under the field, when there is not. */
  error?: string;
}

export function readWaiverDays(draft: string, maxDays: number): WaiverDaysReading {
  const text = draft.trim();
  if (!text) return { error: "Say how many days. There is no permanent waiver." };
  if (!/^\d+$/.test(text)) return { error: "Whole days only." };
  const days = Number(text);
  if (days < 1) return { error: "At least one day." };
  if (maxDays > 0 && days > maxDays) {
    return { error: `This install caps a waiver at ${maxDays} days.` };
  }
  return { days };
}
