import type { ReactNode } from "react";

// Renders process log lines cleanly: ANSI SGR color codes become styled spans,
// and every other escape sequence is dropped instead of showing up as junk
// glyphs.
//
// These are the app's own bytes, which is why this half is the client's. A
// backend's wire format — the runner's marker — is removed by the backend that
// reads it, never here.

// eslint-disable-next-line no-control-regex
const ANSI_SGR = /\x1b\[([0-9;]*)m/g;
// Every escape *except* colour: `renderLogLine` removes these and then parses
// the colours that are left. The final byte excludes `m` — when it did not,
// this took the colours too, before the colour pass ever saw them, and every
// log rendered white.
// eslint-disable-next-line no-control-regex
const ANSI_OTHER = /\x1b\[[0-9;?]*[A-Za-ln-z]|\x1b[^[]/g;

// Muted terminal palette tuned for the `--terminal` log background.
const FG: Record<number, string> = {
  30: "#71717a", 31: "#f87171", 32: "#4ade80", 33: "#facc15",
  34: "#60a5fa", 35: "#c084fc", 36: "#22d3ee", 37: "#e4e4e7",
  90: "#71717a", 91: "#fca5a5", 92: "#86efac", 93: "#fde047",
  94: "#93c5fd", 95: "#d8b4fe", 96: "#67e8f9", 97: "#fafafa",
};

export function stripLogNoise(line: string): string {
  return line.replace(ANSI_SGR, "").replace(ANSI_OTHER, "");
}

/**
 * The tint an unstyled line gets, when the line has not coloured itself.
 *
 * `platform` is Toft speaking rather than the app — the runner's stage
 * marks, the reason a start failed — and it is deliberately distinct from
 * `stderr` rather than sharing it. "The app never started" and "the app started
 * and printed an error" are the two things an editor is trying to tell apart on
 * this pane, and they should be one glance apart, not one reading apart.
 *
 * Raw hex rather than a tone, as the rest of this file is: these sit on the
 * terminal's own background beside ANSI colours the app chose, and a tone token
 * tuned for a card surface does not read there.
 */
const LINE_TINT = {
  stderr: "#fda4af",
  platform: "#93c5fd",
} as const;

export type LogLineTint = keyof typeof LINE_TINT;

/** One log line as styled spans; `tint` colours text the line did not colour
 *  itself. `trailing` goes before the line's newline — a repeat count after it lands
 *  alone on the next line, where it reads as a stray log line. */
export function renderLogLine(
  raw: string,
  key: React.Key,
  tint?: LogLineTint,
  trailing?: ReactNode,
): ReactNode {
  const line = raw.replace(ANSI_OTHER, "");
  const parts: ReactNode[] = [];
  let color: string | null = null;
  let bold = false;
  let last = 0;
  let i = 0;

  const push = (text: string) => {
    if (!text) return;
    const style: React.CSSProperties = {};
    if (color) style.color = color;
    else if (tint) style.color = LINE_TINT[tint];
    if (bold) style.fontWeight = 600;
    parts.push(
      Object.keys(style).length ? <span key={`${key}-${i++}`} style={style}>{text}</span> : text
    );
  };

  for (const m of line.matchAll(ANSI_SGR)) {
    push(line.slice(last, m.index));
    last = m.index! + m[0].length;
    for (const code of (m[1] || "0").split(";").map(Number)) {
      if (code === 0) { color = null; bold = false; }
      else if (code === 1) bold = true;
      else if (code === 22) bold = false;
      else if (code === 39) color = null;
      else if (FG[code]) color = FG[code];
    }
  }
  push(line.slice(last));
  return <span key={key}>{parts}{trailing}{"\n"}</span>;
}
