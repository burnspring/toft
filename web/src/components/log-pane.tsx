import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  ChevronDown,
  ChevronRight,
  ChevronUp,
  Copy,
  Download,
  ExternalLink,
  WrapText,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import { buttonVariants } from "@/components/ui/button-variants";
import { SearchInput } from "@/components/search-input";
import { copyToClipboard } from "@/lib/clipboard";
import { renderLogLine, stripLogNoise, type LogLineTint } from "@/lib/ansi";
import { formatBytes } from "@/lib/bytes";
import {
  defaultFolded,
  firstErrorLine,
  lineHash,
  logSections,
  parseLineHash,
  sectionOf,
} from "@/lib/log-view";
import { formatClock } from "@/lib/time";
import { toneSolid, type Tone } from "@/lib/tone";
import { cn } from "@/lib/utils";

/**
 * The terminal surface, and everything a person reading one needs.
 *
 * There were two of these and they had different defects. The run log could be
 * searched, followed, and told you when its socket had dropped; the build log —
 * four hundred lines of renv traceback, the case where finding a string matters
 * most — could do none of the three, and lived in a modal. Reuse here is not
 * tidiness: one component is how the build log gains search, following and a
 * live state, and how neither of them can lose one again (ADR §87).
 *
 * What stays with the caller is what the caller alone knows: where the lines
 * came from, why there are none, and what its own filters are.
 */
export interface LogPaneLine {
  /** The raw line, ANSI escapes and all. */
  line: string;
  /** When it was printed, where anything stamped it. */
  at?: string;
  tint?: LogLineTint;
}

/** How the caller's connection is doing, said in words and not only in colour. */
export type LogStreamState = "connecting" | "live" | "closed" | "none";

/**
 * How many rows the browser lays out as one block. A block off screen is
 * skipped by `content-visibility`, which is what keeps a fifty-thousand-line
 * log scrollable without a virtualization library: the rows stay in the DOM —
 * so the browser's own find, selection and `scrollIntoView` keep working — and
 * only the blocks near the viewport are laid out and painted.
 */
const CHUNK = 200;
/** `text-xs leading-relaxed`: 12px × 1.625. The estimate a skipped block is sized at. */
const ROW_PX = 19.5;

interface Row extends LogPaneLine {
  /** The line's position in what the caller passed — its number, less one. */
  index: number;
  repeats: number;
}

/** Where the pane is to scroll once the row it names has rendered. */
type PendingScroll = { line: number } | { match: number } | null;

export function LogPane({
  lines,
  label,
  emptyMessage,
  stream = "none",
  onReconnect,
  controls,
  footnote,
  subject,
  downloadName,
  downloadHref,
  rawHref,
  reader = false,
  failed = false,
  paneClassName = "max-h-[600px]",
  narrowed: narrowedByCaller = false,
}: {
  lines: LogPaneLine[];
  /** What a screen reader calls this pane. */
  label: string;
  /** What to say when there is nothing to draw — the caller's fact, not ours. */
  emptyMessage: string;
  stream?: LogStreamState;
  onReconnect?: () => void;
  /** The caller's own controls, beside the search box. */
  controls?: ReactNode;
  /** The caller's sentence under the pane, about retention or where it began. */
  footnote?: ReactNode;
  /** A lower-case noun phrase for the copy toast — "log", "build log". */
  subject: string;
  /** What a file saved from this page is called; `narrowed` when it holds only the matching lines. */
  downloadName: (narrowed: boolean) => string;
  /**
   * Where the server hands over the **whole** log as a file. When given,
   * Download is that, whatever this page received or has filtered (ADR §87).
   * Absent, Download saves the lines passed in.
   */
  downloadHref?: string;
  /** The same log as plain text in a tab of its own, for the browser's own find and save. */
  rawHref?: string;
  /**
   * The full reader: numbered lines whose numbers are links (`#L412`), the
   * builder's `==> ` steps as folds, and a way to the first error. For the one
   * surface with an address — the Logs tab — and not for a window onto a log.
   */
  reader?: boolean;
  /** Whether the log is a failure's: a failed build opens with every step but the last folded. */
  failed?: boolean;
  paneClassName?: string;
  /** Whether the caller's own filters are narrowing what it passed. */
  narrowed?: boolean;
}) {
  const [wrap, setWrap] = useState(false);
  const [search, setSearch] = useState("");
  /** Whether a search hides what does not match, rather than marking what does. */
  const [onlyMatching, setOnlyMatching] = useState(false);
  const paneRef = useRef<HTMLPreElement>(null);

  /**
   * The line somebody was sent to, or chose. Read once from the fragment: a
   * link to `#L412` is a build log somebody wanted read at that line.
   */
  const [target, setTarget] = useState<number | null>(() =>
    reader ? parseLineHash(window.location.hash) : null,
  );
  const pending = useRef<PendingScroll>(target === null ? null : { line: target });

  /**
   * Whether new lines scroll the pane.
   *
   * Derived from where the pane is scrolled rather than from a switch: reading
   * something forty lines back is the act of stopping, and every arriving line
   * used to yank the pane away from it. Scrolling back to the bottom resumes.
   * A pane opened at a line starts stopped there.
   */
  const [following, setFollowing] = useState(target === null);

  const q = search.trim().toLowerCase();

  const rows = useMemo(() => collapseRepeats(lines), [lines]);

  /** The rows that match, by position in `rows`. */
  const matches = useMemo(() => {
    if (!q) return [];
    const out: number[] = [];
    rows.forEach((r, pos) => {
      if (stripLogNoise(r.line).toLowerCase().includes(q)) out.push(pos);
    });
    return out;
  }, [rows, q]);
  const matchSet = useMemo(() => new Set(matches), [matches]);

  /**
   * Which match is current. Held with the query it belongs to, so a new query
   * starts at its own first match without an effect resetting the old one.
   */
  const [cursor, setCursor] = useState({ q: "", pos: 0 });
  const current = cursor.q === q ? cursor.pos : 0;

  const sections = useMemo(
    () => (reader ? logSections(lines.map((l) => l.line)) : []),
    [reader, lines],
  );
  /**
   * Which steps are folded — **derived, with the reader's choices on top**,
   * like the console's collapse. The default is a function of the outcome and
   * of steps that arrive while a build runs, so it is computed rather than
   * stored; `toggled` holds only what somebody opened or closed. The step
   * holding the line somebody was sent to is open too — a link into a folded
   * step would otherwise land on its header and scroll nowhere.
   */
  const [toggled, setToggled] = useState<Map<number, boolean>>(new Map());
  const folded = useMemo(() => {
    const out = defaultFolded(sections, failed);
    if (target !== null) out.delete(sectionOf(sections, target));
    for (const [start, isFolded] of toggled) {
      if (isFolded) out.add(start);
      else out.delete(start);
    }
    return out;
  }, [sections, failed, target, toggled]);
  const sectionEnd = useMemo(
    () => new Map(sections.map((s) => [s.start, s.end])),
    [sections],
  );

  const errorLine = useMemo(
    () => (reader ? firstErrorLine(lines.map((l) => stripLogNoise(l.line))) : -1),
    [reader, lines],
  );

  /**
   * What is drawn. A search that hides shows the matches alone; a search that
   * marks shows everything, folds open, so a match is never inside a step
   * nobody can see. Otherwise a folded step is its header line.
   */
  const visible = useMemo(() => {
    if (q && onlyMatching) return rows.filter((_, pos) => matchSet.has(pos));
    if (q || folded.size === 0) return rows;
    return rows.filter((r) => {
      const s = sectionOf(sections, r.index);
      return s === -1 || s === r.index || !folded.has(s);
    });
  }, [rows, q, onlyMatching, matchSet, folded, sections]);

  const shown = q && onlyMatching ? visible : rows;
  // A gutter only where something stamped the lines. A line nothing marked
  // carries no time, and an invented one would be worse than none.
  const times = useMemo(() => lines.some((l) => l.at), [lines]);
  const digits = String(lines.length).length;

  const plain = useMemo(
    () =>
      shown
        .flatMap((r) => Array(r.repeats).fill(stripLogNoise(r.line)) as string[])
        .join("\n"),
    [shown],
  );
  const narrowed = narrowedByCaller || (q !== "" && onlyMatching);

  // Follow, unless the reader has scrolled away from the bottom. Wrapping is a
  // dependency too: turning it on changes the pane's height, and a followed
  // pane that stayed where it was would be showing the middle of the log.
  useEffect(() => {
    if (!following) return;
    const el = paneRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [visible, following, wrap]);

  /**
   * Scroll to the row that was asked for, once it has rendered.
   *
   * After every render rather than on a dependency, and through a ref rather
   * than state: the row may not exist yet — a stored log still loading, a step
   * that unfolds in this very commit — and the request waits until it does.
   * Declared after the follow effect so that it wins the commit it lands in.
   */
  useEffect(() => {
    const want = pending.current;
    const el = paneRef.current;
    if (!want || !el) return;
    let line: number | undefined;
    if ("line" in want) line = want.line;
    else if (matches.length > 0) line = rows[matches[want.match]]?.index;
    else {
      pending.current = null;
      return;
    }
    if (line === undefined) return;
    const row = el.querySelector<HTMLElement>(`[data-line='${line}']`);
    if (!row) return;
    pending.current = null;
    // Vertical only. `scrollIntoView` also moves a pane that scrolls sideways,
    // and a row as wide as the widest line put the gutter off the left edge.
    // Measured from the rects, not `offsetTop`: each block's containment makes
    // it the row's offset parent.
    el.scrollTop +=
      row.getBoundingClientRect().top - el.getBoundingClientRect().top - el.clientHeight / 2;
  });

  /** Go to a line, opening its step: `folded` leaves the target's step open. */
  function goToLine(line: number) {
    const s = sectionOf(sections, line);
    if (toggled.get(s)) setToggled((m) => new Map(m).set(s, false));
    setTarget(line);
    setFollowing(false);
    pending.current = { line };
  }

  function goToMatch(delta: number) {
    if (matches.length === 0) return;
    const pos = (current + delta + matches.length) % matches.length;
    setCursor({ q, pos });
    setFollowing(false);
    pending.current = { match: pos };
  }

  /** A line's number is its address: the fragment changes, the page does not. */
  function linkLine(line: number) {
    window.history.replaceState(window.history.state, "", lineHash(line));
    setTarget(line);
  }

  /**
   * Save lines this page holds, as plain text.
   *
   * The point of a log file is the sentence "send it to someone" — a failing R
   * restore is four hundred lines naming a shared object, and the person who
   * can read it is not the person looking at the screen. So what lands on disk
   * is the *plain* text: the escapes the pane turns into colour are noise in a
   * file, in a chat message and in an email. And the name says when it is not
   * the whole thing, because four `logs.txt` in one downloads folder are four
   * files nobody can tell apart.
   *
   * **Download is never this when the server can do it** (`downloadHref`): a
   * page holds what it was connected for, and a file built from that was a
   * partial copy saved under a button that promised the log. This is what
   * *Save matching lines* does, and the fallback for a caller with no server
   * copy to offer.
   */
  function save(onlyShown: boolean) {
    const text = onlyShown
      ? plain
      : lines.map((l) => stripLogNoise(l.line)).join("\n");
    const url = URL.createObjectURL(
      new Blob([text], { type: "text/plain;charset=utf-8" }),
    );
    const a = document.createElement("a");
    a.href = url;
    a.download = downloadName(onlyShown);
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
  }

  const currentRow = q && matches.length > 0 ? matches[current] : -1;

  function renderRow(r: Row, pos: number) {
    const isHeader = sectionEnd.has(r.index);
    const isFolded = isHeader && !q && folded.has(r.index);
    const hidden = isFolded ? (sectionEnd.get(r.index) ?? r.index) - r.index - 1 : 0;
    // Before the line's newline, on the line it counts.
    const repeats =
      r.repeats > 1 ? (
        <span className="ml-2 select-none text-terminal-foreground/70">×{r.repeats}</span>
      ) : null;
    const rowPos = q && onlyMatching ? matches[pos] : pos;
    return (
      <span
        key={r.index}
        data-line={r.index}
        className={cn(
          "block px-4",
          // Under "only matching" every row matches, and marking all of them
          // marks nothing.
          q && !onlyMatching && matchSet.has(rowPos) && "bg-terminal-foreground/10",
          (rowPos === currentRow || (target === r.index && !q)) &&
            "bg-terminal-foreground/20",
        )}
      >
        {reader && (
          <a
            href={lineHash(r.index)}
            onClick={(e) => {
              e.preventDefault();
              linkLine(r.index);
            }}
            aria-label={`Link to line ${r.index + 1}`}
            className="mr-3 inline-block select-none text-right text-terminal-foreground/60 hover:text-terminal-foreground hover:underline"
            style={{ width: `${digits}ch` }}
          >
            {r.index + 1}
          </a>
        )}
        {times && (
          <span className="mr-3 select-none text-terminal-foreground/60">
            {(r.at && formatClock(r.at)) || "        "}
          </span>
        )}
        {isHeader && !q ? (
          <button
            type="button"
            aria-expanded={!isFolded}
            onClick={() =>
              setToggled((m) => new Map(m).set(r.index, !isFolded))
            }
            className="mr-1 inline-flex align-middle text-terminal-foreground/70 hover:text-terminal-foreground"
          >
            {isFolded ? (
              <ChevronRight className="h-3 w-3" />
            ) : (
              <ChevronDown className="h-3 w-3" />
            )}
            <span className="sr-only">{isFolded ? "Show this step" : "Hide this step"}</span>
          </button>
        ) : null}
        {isFolded ? (
          <>
            {stripLogNoise(r.line)}
            <span className="ml-2 select-none text-terminal-foreground/60">
              {hidden.toLocaleString()} {hidden === 1 ? "line" : "lines"}
            </span>
            {repeats}
            {"\n"}
          </>
        ) : (
          renderLogLine(r.line, r.index, r.tint, repeats)
        )}
      </span>
    );
  }

  const chunks: Row[][] = [];
  for (let i = 0; i < visible.length; i += CHUNK) chunks.push(visible.slice(i, i + CHUNK));

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <SearchInput
            placeholder="Find in log…"
            onChange={(v) => {
              setSearch(v);
              if (v.trim()) {
                setFollowing(false);
                pending.current = { match: 0 };
              }
            }}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                goToMatch(e.shiftKey ? -1 : 1);
              }
            }}
            className="max-w-xs"
          />
          {q && (
            <span className="flex items-center gap-1 text-2xs text-muted-foreground">
              <span aria-live="polite" className="tabular-nums">
                {matches.length === 0
                  ? "No matches"
                  : `${(current + 1).toLocaleString()} of ${matches.length.toLocaleString()}`}
              </span>
              <Button
                variant="ghost"
                size="icon-xs"
                aria-label="Previous match"
                disabled={matches.length === 0 || onlyMatching}
                onClick={() => goToMatch(-1)}
              >
                <ChevronUp />
              </Button>
              <Button
                variant="ghost"
                size="icon-xs"
                aria-label="Next match"
                disabled={matches.length === 0 || onlyMatching}
                onClick={() => goToMatch(1)}
              >
                <ChevronDown />
              </Button>
              <Button
                variant="ghost"
                size="xs"
                aria-pressed={onlyMatching}
                onClick={() => setOnlyMatching(!onlyMatching)}
              >
                {onlyMatching ? "Show all lines" : "Only matching lines"}
              </Button>
            </span>
          )}
          {stream !== "none" && (
            <StreamState state={stream} onReconnect={onReconnect} />
          )}
          {controls}
        </div>
        <div className="flex flex-wrap items-center gap-1">
          {errorLine >= 0 && (
            <Button variant="ghost" size="sm" onClick={() => goToLine(errorLine)}>
              Jump to first error
            </Button>
          )}
          <Button
            variant="ghost"
            size="sm"
            aria-pressed={wrap}
            onClick={() => setWrap(!wrap)}
          >
            <WrapText />
            {wrap ? "Don’t wrap" : "Wrap lines"}
          </Button>
          <Button
            variant="ghost"
            size="sm"
            disabled={shown.length === 0}
            onClick={() => copyToClipboard(plain, subject)}
          >
            <Copy />
            Copy
          </Button>
          {narrowed && (
            <Button
              variant="ghost"
              size="sm"
              disabled={shown.length === 0}
              onClick={() => save(true)}
            >
              <Download />
              Save matching lines
            </Button>
          )}
          {downloadHref ? (
            <a
              href={downloadHref}
              download
              className={buttonVariants({ variant: "ghost", size: "sm" })}
            >
              <Download />
              Download
            </a>
          ) : (
            <Button
              variant="ghost"
              size="sm"
              disabled={lines.length === 0}
              onClick={() => save(false)}
            >
              <Download />
              Download
            </Button>
          )}
          {rawHref && (
            <a
              href={rawHref}
              target="_blank"
              rel="noopener noreferrer"
              className={buttonVariants({ variant: "ghost", size: "sm" })}
            >
              <ExternalLink />
              View raw
            </a>
          )}
        </div>
      </div>

      <div className="overflow-hidden rounded-lg border border-border/60 bg-terminal shadow-sm">
        <pre
          ref={paneRef}
          // A scrollable element with no tab stop and no focusable children
          // cannot be scrolled by keyboard at all — WCAG 2.1.1 — and this pane
          // is the primary evidence surface for a crashed app and a failed
          // build. `region` and not `log`: `log` carries an implicit live
          // region in some readers, and arriving lines announcing themselves is
          // not what somebody reading history wants.
          tabIndex={0}
          role="region"
          aria-label={label}
          onScroll={(e) => {
            const el = e.currentTarget;
            // A line's worth of slack, so following survives the rounding a
            // smooth scroll leaves behind.
            setFollowing(el.scrollHeight - el.scrollTop - el.clientHeight < 24);
          }}
          className={cn(
            "overflow-y-auto py-4 font-mono text-xs leading-relaxed text-terminal-foreground focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none",
            paneClassName,
            wrap ? "whitespace-pre-wrap break-words" : "overflow-x-auto",
          )}
        >
          <code className={cn("block", !wrap && "w-max min-w-full")}>
            {visible.length > 0
              ? chunks.map((chunk, c) => (
                  <span
                    key={c}
                    className="block"
                    style={{
                      contentVisibility: "auto",
                      containIntrinsicSize: `auto ${Math.ceil(chunk.length * ROW_PX)}px`,
                    }}
                  >
                    {chunk.map((r, i) => renderRow(r, c * CHUNK + i))}
                  </span>
                ))
              : (
                  <span className="block px-4">
                    {lines.length > 0 || narrowedByCaller
                      ? "No lines match the current filter."
                      : emptyMessage}
                  </span>
                )}
          </code>
        </pre>
      </div>

      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 text-2xs text-muted-foreground">
        <span>
          {shown.length > 0 && (
            <>
              {plainLineCount(shown).toLocaleString()}{" "}
              {plainLineCount(shown) === 1 ? "line" : "lines"} ·{" "}
              {formatBytes(new Blob([plain]).size)}
              {footnote ? " · " : null}
            </>
          )}
          {footnote}
        </span>
        {!following && (
          <Button
            variant="ghost"
            size="sm"
            className="h-6 px-2 text-2xs"
            onClick={() => {
              const el = paneRef.current;
              if (el) el.scrollTop = el.scrollHeight;
              setFollowing(true);
            }}
          >
            Jump to latest
          </Button>
        )}
      </div>
    </div>
  );
}

/** How many lines the rows stand for, repeats counted. */
function plainLineCount(rows: Row[]): number {
  let n = 0;
  for (const r of rows) n += r.repeats;
  return n;
}

/**
 * Whether the pane above is live, in text as well as colour.
 *
 * The dot alone would be a colour carrying the whole meaning, which is the one
 * thing this product's rules say a state may never be — and it is the state
 * where being wrong costs the most, because a person reads a still pane as
 * "nothing is happening" rather than "nothing is arriving".
 */
function StreamState({
  state,
  onReconnect,
}: {
  state: LogStreamState;
  onReconnect?: () => void;
}) {
  const tone: Tone =
    state === "live" ? "ok" : state === "connecting" ? "info" : "neutral";
  const label =
    state === "live" ? "Live" : state === "connecting" ? "Connecting…" : "Not live";
  return (
    <span className="flex shrink-0 items-center gap-1.5 text-2xs text-muted-foreground">
      <span
        className={cn("h-1.5 w-1.5 rounded-full", toneSolid[tone])}
        aria-hidden="true"
      />
      {label}
      {state === "closed" && onReconnect && (
        <Button
          variant="ghost"
          size="sm"
          className="h-6 px-1.5 text-2xs"
          onClick={onReconnect}
        >
          Reconnect
        </Button>
      )}
    </span>
  );
}

/**
 * Consecutive identical lines, as one line and a count.
 *
 * A workload stuck in a retry loop prints the same sentence until somebody
 * stops it, and a pane holding two thousand of them holds one fact. Collapsing
 * only *adjacent* repeats keeps the log a log: nothing is reordered, nothing far
 * apart is merged, and a line that comes back after something else happened is a
 * separate row — which is the shape of the evidence somebody is reading for.
 *
 * A blank line is never collapsed. Build output is full of them as spacing, and
 * "×7" beside an empty row counts something nobody wrote.
 *
 * The first occurrence's timestamp is the one kept, because it is the one that
 * answers "when did this start".
 */
function collapseRepeats(lines: LogPaneLine[]): Row[] {
  const out: Row[] = [];
  lines.forEach((l, index) => {
    const previous = out[out.length - 1];
    if (
      previous &&
      l.line.trim() !== "" &&
      previous.line === l.line &&
      previous.tint === l.tint
    ) {
      previous.repeats += 1;
      return;
    }
    out.push({ ...l, index, repeats: 1 });
  });
  return out;
}
