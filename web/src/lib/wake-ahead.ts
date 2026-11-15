import { api } from "@/lib/api";

/**
 * Wake a sleeping app while the pointer or the keyboard is on its Open button,
 * so the wake is already under way when the click lands (ADR §123).
 *
 * Only for `sleeping`, and once per app per page: hovering back and forth is
 * not twenty wakes, and the server joins a wake in flight anyway. It is a hint
 * and says nothing — no toast, because nobody asked for it, and a failure is
 * the gateway's to show the person who arrives. Opening the app's page does
 * *not* wake it: that is somebody reading settings, and idle sleep is there to
 * save exactly that.
 */
const asked = new Set<string>();

export function wakeAhead(app: { id: string; status?: string }) {
  if (app.status !== "sleeping" || asked.has(app.id)) return;
  asked.add(app.id);
  api.wakeAhead(app.id).catch(() => undefined);
}

/** The props an Open link spreads to wake its app ahead of the click. */
export function wakeAheadProps(app: { id: string; status?: string }) {
  const wake = () => wakeAhead(app);
  return { onPointerEnter: wake, onFocus: wake };
}
