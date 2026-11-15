import { describe, expect, it } from "vitest";

import { scoreCommand, scoreText } from "./command-score";

describe("scoreText", () => {
  it("ranks a literal match above a scattered subsequence", () => {
    // The measured defect: typing `idle` returned four apps matching only as a
    // subsequence above *Sleep idle apps after*, the one entry containing the
    // string — because groups render in fixed DOM order and a settings row
    // could never outrank an app.
    const setting = scoreText("Sleep idle apps after", "idle");
    const app = scoreText("Finance dashboard quarterly ledger", "idle");
    expect(app).toBeGreaterThan(0); // it does match, faintly
    expect(setting).toBeGreaterThan(app);
  });

  it("puts an exact label first", () => {
    expect(scoreText("gallery", "gallery")).toBeGreaterThan(
      scoreText("gallery deploy mode", "gallery"),
    );
  });

  it("prefers a word that starts with the query to one that contains it", () => {
    expect(scoreText("Deploy keys", "key")).toBeGreaterThan(
      scoreText("Monkey business", "key"),
    );
  });

  it("prefers a shorter label over a longer one that says the same thing", () => {
    expect(scoreText("Storage", "storage")).toBeGreaterThan(
      scoreText("Storage retention sweep interval in minutes", "storage"),
    );
  });

  it("finds an initialism", () => {
    expect(scoreText("Sleep idle apps after", "sia")).toBeGreaterThan(0);
  });

  it("finds a label by its words in any order", () => {
    // Measured: "idle sleep" — the order a person who knows the setting by
    // that name types it — answered "No results found", because every band
    // wanted the query as one run of characters.
    const anyOrder = scoreText("Sleep idle apps after", "idle sleep");
    const inOrder = scoreText("Sleep idle apps after", "sleep idle");
    expect(anyOrder).toBeGreaterThanOrEqual(0.8);
    expect(inOrder).toBeGreaterThanOrEqual(0.8);
    // Each word has to start a word of its own: "sleep sleep" is one word, twice.
    expect(scoreText("Sleep idle apps after", "sleep sleep")).toBeLessThan(0.8);
    // And every word has to be there — a second word that matches nothing is
    // not a looser query, it is a different one.
    expect(scoreText("Sleep idle apps after", "idle storage")).toBe(0);
  });

  it("is zero when the letters are not there in order", () => {
    expect(scoreText("Sleep idle apps after", "zzz")).toBe(0);
    // Out of order: the `r` is the last character, so nothing can follow it.
    expect(scoreText("Sleep idle apps after", "rs")).toBe(0);
  });

  it("matches everything on an empty query, so browsing keeps the original order", () => {
    expect(scoreText("anything", "")).toBe(1);
    expect(scoreText("anything", "   ")).toBe(1);
  });

  it("never crosses a band on closeness alone", () => {
    // The bands are the ranking; the tie-break inside one may not overturn
    // them, which is what a bonus of up to 0.14 across a 0.1 gap did.
    // A substring buried at the end of a short label must still lose to a word
    // prefix in a long one: the bands are the ranking, and the tie-break
    // inside a band may not overturn them.
    const wordPrefix = scoreText(
      "a very long entry indeed with many words about keys and things",
      "key",
    );
    const substring = scoreText("monkey", "key");
    expect(wordPrefix).toBeGreaterThan(substring);
  });
});

describe("scoreCommand", () => {
  // A settings entry's searchable text carries the paragraph explaining the
  // setting, and a paragraph contains almost any subsequence. Merged into one
  // string, typing `storage` returned "SMTP host" and "Collector endpoint"
  // above every real match.
  const prose =
    "Where mail goes. An OpenTelemetry collector that speaks OTLP over HTTP; " +
    "the relay this install sends through, and the credential it presents.";

  it("never lets a subsequence through the prose outrank a real match", () => {
    const real = scoreCommand("Storage", "admin apps", "storage");
    const noise = scoreCommand("SMTP host", prose, "storage");
    expect(noise).toBe(0);
    expect(real).toBeGreaterThan(0);
  });

  it("still finds an entry by a synonym, below anything matching by name", () => {
    const bySynonym = scoreCommand("Your storage", "buckets files uploads", "uploads");
    const byName = scoreCommand("Uploads and archives", "", "uploads");
    expect(bySynonym).toBeGreaterThan(0);
    expect(byName).toBeGreaterThan(bySynonym);
  });

  it("scores the label when both could match", () => {
    expect(scoreCommand("Documentation", "docs guides manual", "documentation"))
      .toBe(scoreText("Documentation", "documentation"));
  });
});
