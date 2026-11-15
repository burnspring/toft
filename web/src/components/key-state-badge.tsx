import { Badge } from "@/components/ui/badge";
import { toneSurface, type Tone } from "@/lib/tone";
import type { KeyState } from "@/lib/api";

/** One vocabulary for both credential classes: an app key and a personal key
 * are different things, but "disabled" means the same about both.
 *
 * `expired` is danger and `disabled` is attention, which is the distinction the
 * tones exist to carry: one stopped working on its own and somebody has to mint
 * a replacement, the other is an administrator's decision holding. Before the
 * tone table these were `destructive` and a hand-written amber, from two
 * different palettes, and the difference read as an accident. */
const KEY_STATE: Record<KeyState, { label: string; tone: Tone }> = {
  disabled: { label: "Disabled", tone: "attention" },
  expired: { label: "Expired", tone: "danger" },
  active: { label: "Active", tone: "ok" },
  /** A deploy key's own state, and the only one of the four that cannot be
   *  undone. Neutral rather than danger: expired is a thing that happened to a
   *  credential somebody still wants, revoked is a decision that has already
   *  been carried out. Nothing is wrong with a revoked key — it is finished. */
  revoked: { label: "Revoked", tone: "neutral" },
};

export function KeyStateBadge({ state }: { state: KeyState }) {
  const { label, tone } = KEY_STATE[state] ?? KEY_STATE.active;
  return (
    <Badge variant="outline" className={toneSurface[tone]}>
      {label}
    </Badge>
  );
}
