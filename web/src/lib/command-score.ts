/**
 * How well a palette entry answers what somebody typed.
 *
 * `cmdk` scores and sorts **within a group**, and groups render in DOM order,
 * so an entry in a later group can never outrank one in an earlier group
 * however much better it matches. Measured on a live install: typing `idle`
 * returned four apps that match only as a scattered subsequence —
 * `Fin(i)ance (d)ashboar(d) … ` — above *Sleep idle apps after*, the one entry
 * containing the string, because Apps is rendered first. Long descriptive app
 * names, which this product is for, make that true of every short query.
 *
 * So the palette scores every entry with this and orders the whole list by the
 * answer, keeping each group's name as a label on its rows.
 *
 * The scale is deliberately coarse — five bands, not a continuum — because
 * what matters is that a literal match beats an initialism and an initialism
 * beats a scattered subsequence. Within a band the tie is broken by where the
 * match starts and by how much of the text it covers, so a short exact label
 * outranks a long one that merely contains the word.
 *
 * 0 means "not a match at all", and the caller drops the row.
 */

const EXACT = 1;
const PREFIX = 0.9;
const WORD_PREFIX = 0.8;
const SUBSTRING = 0.6;
const INITIALS = 0.4;
const SUBSEQUENCE = 0.2;

/** Where the searchable text of an entry is split into words. */
function words(text: string): string[] {
  return text.split(/[^a-z0-9]+/i).filter(Boolean);
}

/**
 * The whole score for one entry: its label at full weight, its synonyms at a
 * discount.
 *
 * The two are scored apart because a settings entry's searchable text includes
 * the paragraph explaining what the setting does, and **a paragraph contains
 * almost any subsequence**. Measured with them merged: typing `storage`
 * returned "SMTP host", "Reply-to" and "Collector endpoint" above every real
 * match, each of them scoring on a scattered subsequence through its own prose.
 *
 * So a synonym match is capped below a label match, and the subsequence band —
 * the loosest one, which exists for a typo in a name somebody half remembers —
 * is not offered to the synonyms at all.
 */
export function scoreCommand(label: string, keywords: string, query: string): number {
  const direct = scoreText(label, query);
  if (direct > 0) return direct;
  const synonym = scoreText(keywords, query);
  if (synonym <= SUBSEQUENCE + 0.05) return 0;
  return Math.min(synonym, SUBSTRING - 0.01);
}

/**
 * @param text  one searchable string
 * @param query what was typed
 */
export function scoreText(text: string, query: string): number {
  const t = text.toLowerCase().trim();
  const q = query.toLowerCase().trim();
  if (!q) return EXACT;
  if (!t) return 0;

  if (t === q) return EXACT;

  // Closeness within a band: a match near the front of a short label is a
  // better answer than one buried in a long one, and a query that covers most
  // of the label is a better answer than one that covers a tenth of it.
  //
  // **The bonus is capped below the gap between bands (0.1).** The bands are
  // the ranking; this only breaks ties inside one, and a tie-break that can
  // overturn a band is a second ranking nobody chose.
  const band = (base: number, at: number) =>
    base +
    0.06 * (1 - at / Math.max(t.length, 1)) +
    0.03 * Math.min(1, q.length / t.length);

  if (t.startsWith(q)) return band(PREFIX, 0);

  const ws = words(t);
  let offset = 0;
  for (const w of ws) {
    const at = t.indexOf(w, offset);
    offset = at + w.length;
    if (w === q) return band(PREFIX, at);
    if (w.startsWith(q)) return band(WORD_PREFIX, at);
  }

  const at = t.indexOf(q);
  if (at >= 0) return band(SUBSTRING, at);

  // Several words, each the start of a word of the text, in any order: "idle
  // sleep" finds *Sleep idle apps after*. Measured on a live install: typed in
  // that order — the order a person who knows the setting as "idle sleep" types
  // it — the palette answered "No results found", because every band above
  // wants the query as one run of characters and the subsequence below needs
  // its letters in order. Word-prefix band, because that is what each word is;
  // the tie-break is where the first matched word sits.
  const qs = words(q);
  if (qs.length > 1) {
    const taken = new Set<number>();
    let first = -1;
    const all = qs.every((qw) => {
      const i = ws.findIndex((w, k) => !taken.has(k) && w.startsWith(qw));
      if (i < 0) return false;
      taken.add(i);
      const pos = t.indexOf(ws[i]);
      first = first < 0 ? pos : Math.min(first, pos);
      return true;
    });
    if (all) return band(WORD_PREFIX, first);
  }

  // The first letters of consecutive words: "sia" finds "Sleep idle apps".
  const initials = ws.map((w) => w[0]).join("");
  const initialsAt = initials.indexOf(q);
  if (q.length > 1 && initialsAt >= 0) return band(INITIALS, initialsAt);

  // Last and lowest: the letters in order, anywhere. This is the band that was
  // outranking literal matches, and it earns its place only because a typo or
  // a half-remembered name lands here and nowhere else.
  let i = 0;
  for (const ch of t) {
    if (ch === q[i]) i++;
    if (i === q.length) return SUBSEQUENCE + 0.05 * (q.length / t.length);
  }
  return 0;
}
