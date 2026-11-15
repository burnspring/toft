import { isRerenderable } from "@/lib/rerender";
// Framework display metadata, keyed by the detector's framework identifiers
// (internal/builder/detector.go).

export interface FrameworkMeta {
  label: string;
  /**
   * Brand-ish dot color, muted to sit inside a neutral badge.
   *
   * **Not the brand's own colour, and not meant to be.** A 6px dot inside a
   * neutral chip needs a value chosen for that job; a 28px logo needs the real
   * one. The landing page's marks therefore read `framework-marks.ts`, which
   * carries the vendor's hex beside the vendor's path, and the two files are
   * allowed to differ — Node and Flask do. Making them agree would mean either
   * a wrong logo or a restyled badge on every app row.
   */
  color: string;
}

const frameworks: Record<string, FrameworkMeta> = {
  nextjs: { label: "Next.js", color: "#000000" },
  node: { label: "Node.js", color: "#3c873a" },
  go: { label: "Go", color: "#00add8" },
  flask: { label: "Flask", color: "#e5544b" },
  fastapi: { label: "FastAPI", color: "#009688" },
  streamlit: { label: "Streamlit", color: "#ff4b4b" },
  python: { label: "Python", color: "#3776ab" },
  // Jupyter's orange, muted for the chip. A `notebook` app is not the Jupyter
  // project any more than a `static` app is the HTML5 logo — it is a document
  // executed at build time, from a `.ipynb` or a `.qmd` — but the dot is doing
  // the same job here as everywhere else in this table.
  notebook: { label: "Notebook", color: "#f37726" },
  // marimo publishes no single-path mark, so this colour is chosen rather than
  // borrowed. It is the product's own blue, and `framework-marks.ts` therefore
  // has no entry to disagree with it.
  marimo: { label: "marimo", color: "#1e40af" },
  // Shiny's own blue. A Shiny app is a *process* — one R process serving every
  // viewer — which is what separates it from the two R frameworks that render a
  // document at build time.
  shiny: { label: "Shiny", color: "#3c7cb5" },
  // plumber publishes no single-path mark, so this is chosen rather than
  // borrowed, on marimo's terms. Lowercase, because that is how the project
  // spells itself and how the detector identifies it.
  plumber: { label: "plumber", color: "#1d7874" },
  // The two R documents. Both render at build time and are served as files, so
  // they are the R family's answer to `notebook` — and they carry R's own blue,
  // because unlike Shiny and plumber neither project has a colour of its own.
  rmarkdown: { label: "R Markdown", color: "#276dc3" },
  quarto: { label: "Quarto", color: "#4b8bbe" },
  // Slate, not HTML's orange: at 6px beside a status pill the orange read as a
  // red error dot. A static app is not a language, so it gets no brand colour.
  static: { label: "Static", color: "#64748b" },
  // Batch apps (ADR §107) carry their language's colour: what they are is a
  // program in that language with no server around it, and the label says so.
  "batch-node": { label: "Batch · Node.js", color: "#3c873a" },
  "batch-python": { label: "Batch · Python", color: "#3776ab" },
  "batch-go": { label: "Batch · Go", color: "#00add8" },
  "batch-r": { label: "Batch · R", color: "#276dc3" },
};

/** Whether an app is a batch app (ADR §107): it runs jobs and serves nothing,
 * so it has no address to open and no service to start or stop. */
export function isBatchFramework(framework: string | undefined): boolean {
  return (framework ?? "").startsWith("batch-");
}

/** The three kinds of app, by what answers a request — `builder.AppKind` in
 *  internal/builder/kind.go is the same table. Jobs are not a kind: any app may
 *  declare them. */
export type AppKind = "serving" | "rendered" | "batch";

export function appKind(framework: string | undefined): AppKind {
  if (isBatchFramework(framework)) return "batch";
  if (framework === "static" || isRerenderable(framework)) return "rendered";
  return "serving";
}

/** What the census and the apps list's `?kind=` chip call each kind, and
 *  `jobs`, the overlapping fourth. */
export const APP_KIND_LABEL: Record<AppKind | "jobs", string> = {
  serving: "Running apps",
  rendered: "Rendered and static",
  batch: "Batch",
  jobs: "Has jobs",
};

/**
 * Whether this table actually knows the identifier.
 *
 * `frameworkMeta` deliberately never returns null for a non-empty string — an
 * unrecognised framework renders as its own identifier in a grey badge rather
 * than as a hole — which means its answer cannot be used to check that a name
 * is in the table. This can, and `landing-frameworks.test.ts` is why it exists:
 * `marimo` is spelled exactly like its detector identifier, so "the label
 * differs from the id" stopped being a usable proxy for "the table has heard
 * of it".
 */
export function hasFrameworkMeta(framework: string): boolean {
  return framework in frameworks;
}

export function frameworkMeta(framework?: string): FrameworkMeta | null {
  if (!framework) return null;
  return frameworks[framework] ?? { label: framework, color: "#6b7280" };
}

/** Deterministic accent hue (0-360) from a string, for per-app identity colors. */
export function accentHue(seed: string): number {
  // FNV-1a with a final avalanche so near-identical seeds ("g-1", "g-2")
  // still land on clearly different hues.
  let h = 2166136261 >>> 0;
  for (let i = 0; i < seed.length; i++) {
    h ^= seed.charCodeAt(i);
    h = Math.imul(h, 16777619) >>> 0;
  }
  h ^= h >>> 13;
  h = Math.imul(h, 0x5bd1e995) >>> 0;
  h ^= h >>> 15;
  return h % 360;
}
