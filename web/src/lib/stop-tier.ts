/**
 * Whether stopping this app needs `StopAppDialog`.
 *
 * A group-owned app has an empty `owner_id`, so it confirms — and that is right
 * rather than a compromise: a group-owned app is by construction shared with
 * other people, so "it is somebody else's users who notice" holds there too.
 * No membership data, no new field on the wire.
 *
 * Start and restart stay unconfirmed everywhere: nothing goes dark on a
 * restart.
 */
export function stopNeedsConfirm(
  ownerId: string | undefined,
  userId: string | undefined,
): boolean {
  return !ownerId || !userId || ownerId !== userId;
}
