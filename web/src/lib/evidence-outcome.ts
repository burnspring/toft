import type { Tone } from "@/lib/tone";

/** The four outcomes, and `absent` is the one carrying an argument.
 *
 *  A capability this install does not offer is a *correct* install, so it is
 *  `neutral` — a recorded fact with no valence. Colouring it amber would be
 *  telling an assessor, who does not know which capabilities are optional, that
 *  the install is broken for being smaller than it could be.
 *
 *  `refused` is `attention` rather than `danger` because it is an access
 *  boundary working, not an outage. */
export const OUTCOME_TONE: Record<string, Tone> = {
  ok: "ok",
  absent: "neutral",
  refused: "attention",
  error: "danger",
};

export const OUTCOME_LABEL: Record<string, string> = {
  ok: "Collected",
  absent: "Capability absent",
  refused: "Refused",
  error: "Couldn’t read",
};
