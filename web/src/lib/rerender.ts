/**
 * Whether an app has a rendered document a schedule can re-run: a notebook, an
 * R Markdown document, or a Quarto document under knitr.
 *
 * The mirror of `builder.Rerenderable`, which the API and the scheduler ask —
 * the list is the specification, and a document framework added there is added
 * here, or the schedule exists and nobody can see it.
 */
const RERENDERABLE = new Set(["notebook", "rmarkdown", "quarto"]);

export function isRerenderable(framework: string | null | undefined): boolean {
  return !!framework && RERENDERABLE.has(framework);
}
