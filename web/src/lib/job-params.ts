/**
 * The Run now form's rules, out of the component so they can be tested.
 *
 * The install holds a start to the job's declared params (`param_required`,
 * `param_type`, `param_choice`); this mirrors that check so a person meets the
 * refusal under the field, before the request, in the same words.
 */

import type { JobParamSpec } from "@/lib/api";

/** What a field holds while it is being edited: text for every type but bool. */
export type ParamDraft = Record<string, string | boolean>;

/** The declared params in a stable order — required ones first, then by name. */
export function orderedParams(
  spec: Record<string, JobParamSpec> | null | undefined,
): [string, JobParamSpec][] {
  return Object.entries(spec ?? {}).sort(([a, pa], [b, pb]) => {
    if (!!pa.required !== !!pb.required) return pa.required ? -1 : 1;
    return a.localeCompare(b);
  });
}

/** The form's first state: every default filled in, a bool off unless it defaults on. */
export function initialParamDraft(spec: Record<string, JobParamSpec> | null | undefined): ParamDraft {
  const out: ParamDraft = {};
  for (const [name, p] of orderedParams(spec)) {
    if (p.type === "bool") out[name] = p.default === true;
    else out[name] = p.default === undefined || p.default === null ? "" : String(p.default);
  }
  return out;
}

/**
 * One field's value as the job will receive it, or the sentence that refuses
 * it. `undefined` value with no error means "left out" — legal for an optional
 * param, which the job then sees at its default or not at all.
 */
export function coerceParam(
  name: string,
  spec: JobParamSpec,
  raw: string | boolean | undefined,
): { value?: string | number | boolean; error?: string } {
  if (spec.type === "bool") return { value: raw === true };
  const text = typeof raw === "string" ? raw.trim() : "";
  if (text === "") {
    return spec.required ? { error: `${name} is required.` } : {};
  }
  let value: string | number;
  if (spec.type === "int") {
    if (!/^[-+]?\d+$/.test(text)) return { error: `${name} must be a whole number.` };
    value = Number(text);
  } else if (spec.type === "float") {
    const n = Number(text);
    if (!Number.isFinite(n)) return { error: `${name} must be a number.` };
    value = n;
  } else {
    value = text;
  }
  if (spec.choices && spec.choices.length > 0 && !spec.choices.some((c) => String(c) === String(value))) {
    return { error: `${name} must be one of ${spec.choices.map(String).join(", ")}.` };
  }
  return { value };
}

/** Every field checked: the params to send, and the refusals by field. */
export function collectParams(
  spec: Record<string, JobParamSpec> | null | undefined,
  draft: ParamDraft,
): { params: Record<string, string | number | boolean>; errors: Record<string, string> } {
  const params: Record<string, string | number | boolean> = {};
  const errors: Record<string, string> = {};
  for (const [name, p] of orderedParams(spec)) {
    const { value, error } = coerceParam(name, p, draft[name]);
    if (error) errors[name] = error;
    else if (value !== undefined) params[name] = value;
  }
  return { params, errors };
}

/** A word as a POSIX shell would read it back unchanged. */
export function shellQuote(word: string): string {
  if (word !== "" && /^[A-Za-z0-9_./:@%+=,-]+$/.test(word)) return word;
  return `'${word.replace(/'/g, `'\\''`)}'`;
}

/**
 * The `toft job run` line that does what the form is about to do. Only the
 * params that differ from their defaults are written, which is what a person
 * would type; `--for` takes the instant in RFC 3339.
 */
export function jobRunCommand(
  job: string,
  opts: {
    app?: string;
    spec?: Record<string, JobParamSpec> | null;
    params?: Record<string, string | number | boolean>;
    scheduledFor?: string;
  } = {},
): string {
  const words = ["toft", "job", "run", shellQuote(job)];
  if (opts.app) words.push("--app", shellQuote(opts.app));
  for (const [name, value] of Object.entries(opts.params ?? {})) {
    const declared = opts.spec?.[name]?.default;
    if (declared !== undefined && String(declared) === String(value)) continue;
    words.push("--param", shellQuote(`${name}=${String(value)}`));
  }
  if (opts.scheduledFor) words.push("--for", shellQuote(opts.scheduledFor));
  return words.join(" ");
}
