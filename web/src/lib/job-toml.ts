/**
 * The New job helper's output: a `[jobs.<name>]` block for toft.toml.
 *
 * Jobs are declared in toft.toml and nowhere else, so the helper saves nothing
 * — it writes the text a developer pastes. The checks here mirror the
 * manifest parser (`internal/builder/manifest.go`) so a block this produces is
 * one the next deploy accepts; the install's own ceilings are checked at
 * deploy, against settings this page cannot read.
 */

import type { JobParamType } from "@/lib/api";

export interface ParamRow {
  name: string;
  type: JobParamType;
  required: boolean;
  /** As typed; empty is no default. */
  default: string;
  /** Comma-separated, as typed; empty is no choices. */
  choices: string;
  help: string;
}

export interface JobDraft {
  name: string;
  /** "command" writes `command = "…"`; "args" writes `args = [...]`, one per line typed. */
  mode: "command" | "args";
  command: string;
  /** One argument per line. */
  args: string;
  schedule: string;
  timezone: string;
  /** "45m", "1h30m", or a whole number of seconds; empty is the install default. */
  timeout: string;
  /** MB; empty is the install default. */
  memory: string;
  /** "" leaves the default (queue). Only written with a schedule. */
  overlap: "" | "queue" | "skip";
  /** "" leaves the default (none). */
  notify: "" | "none" | "failures" | "always";
  params: ParamRow[];
}

export const EMPTY_DRAFT: JobDraft = {
  name: "",
  mode: "command",
  command: "",
  args: "",
  schedule: "",
  timezone: "",
  timeout: "",
  memory: "",
  overlap: "",
  notify: "",
  params: [],
};

export function emptyParamRow(): ParamRow {
  return { name: "", type: "string", required: false, default: "", choices: "", help: "" };
}

/** The manifest's rule: lowercase letters, digits and inner dashes, up to 60. */
export function jobNameProblem(name: string): string | null {
  if (name === "") return "A job needs a name.";
  if (name.length > 60 || !/^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(name)) {
    return "Use lowercase letters, digits and inner dashes, up to 60 characters.";
  }
  return null;
}

/** Seconds a timeout names, or null when it is not one toft.toml accepts. */
export function timeoutSeconds(v: string): number | null {
  const t = v.trim();
  if (/^\d+$/.test(t)) return Number(t);
  const m = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$/.exec(t);
  if (!m || t === "") return null;
  return Number(m[1] ?? 0) * 3600 + Number(m[2] ?? 0) * 60 + Number(m[3] ?? 0);
}

export function timeoutProblem(v: string): string | null {
  if (v.trim() === "") return null;
  const secs = timeoutSeconds(v);
  if (secs === null) return "Write a duration like 45m or 2h, or a number of seconds.";
  if (secs < 1 || secs > 7 * 86400) return "Must be between 1 second and 7 days.";
  return null;
}

export function memoryProblem(v: string): string | null {
  const t = v.trim();
  if (t === "") return null;
  if (!/^\d+$/.test(t)) return "Write a whole number of MB.";
  const n = Number(t);
  if (n < 64 || n > 65536) return "Must be between 64 and 65536 MB.";
  return null;
}

/** The literal a value of `type` is written as, or null when it is not one. */
function literal(type: JobParamType, raw: string): string | null {
  const t = raw.trim();
  switch (type) {
    case "int":
      return /^[-+]?\d+$/.test(t) ? String(Number(t)) : null;
    case "float": {
      if (t === "" || !Number.isFinite(Number(t))) return null;
      const s = String(Number(t));
      return /[.eE]/.test(s) ? s : `${s}.0`;
    }
    case "bool":
      return t === "true" || t === "false" ? t : null;
    default:
      return tomlString(raw);
  }
}

const TYPE_WORDS: Record<JobParamType, string> = {
  string: "text",
  int: "a whole number",
  float: "a number",
  bool: "true or false",
};

function splitChoices(raw: string): string[] {
  return raw.split(",").map((c) => c.trim()).filter((c) => c !== "");
}

/** Everything wrong with one param row, or null. */
export function paramProblem(row: ParamRow, others: ParamRow[]): string | null {
  if (row.name.trim() === "") return "A param needs a name.";
  if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(row.name.trim())) {
    return "Use letters, digits and underscores, starting with a letter.";
  }
  if (others.some((o) => o !== row && o.name.trim() === row.name.trim())) {
    return `“${row.name.trim()}” is named twice.`;
  }
  if (row.required && row.default.trim() !== "") {
    return "A required param has no default — a default means it may be left out.";
  }
  const choices = splitChoices(row.choices);
  for (const c of choices) {
    if (literal(row.type, c) === null) return `Choice “${c}” isn’t ${TYPE_WORDS[row.type]}.`;
  }
  if (row.default.trim() !== "") {
    if (literal(row.type, row.default) === null) return `The default isn’t ${TYPE_WORDS[row.type]}.`;
    if (choices.length > 0 && !choices.includes(row.default.trim())) {
      return "The default is not one of the choices.";
    }
  }
  return null;
}

/** A TOML basic string. JSON's escapes are a subset TOML reads the same way. */
export function tomlString(s: string): string {
  return JSON.stringify(s);
}

function tomlKey(k: string): string {
  return /^[A-Za-z0-9_-]+$/.test(k) ? k : tomlString(k);
}

/** The block, written in the order the documentation writes it. */
export function generateJobToml(d: JobDraft): string {
  const name = d.name.trim() || "my-job";
  const lines: string[] = [`[jobs.${tomlKey(name)}]`];
  if (d.mode === "args") {
    const args = d.args.split("\n").map((a) => a.trim()).filter((a) => a !== "");
    lines.push(`args = [${args.map(tomlString).join(", ")}]`);
  } else {
    lines.push(`command = ${tomlString(d.command.trim())}`);
  }
  const timeout = d.timeout.trim();
  if (timeout !== "") {
    lines.push(`timeout = ${/^\d+$/.test(timeout) ? timeout : tomlString(timeout)}`);
  }
  if (d.memory.trim() !== "") lines.push(`memory = ${Number(d.memory.trim())}`);
  const schedule = d.schedule.trim().replace(/\s+/g, " ");
  if (schedule !== "") {
    lines.push(`schedule = ${tomlString(schedule)}`);
    const zone = d.timezone.trim();
    if (zone !== "" && zone !== "UTC") lines.push(`timezone = ${tomlString(zone)}`);
    if (d.overlap !== "") lines.push(`overlap = ${tomlString(d.overlap)}`);
  }
  if (d.notify !== "") lines.push(`notify = ${tomlString(d.notify)}`);

  for (const p of d.params) {
    const pname = p.name.trim();
    if (pname === "") continue;
    lines.push("", `[jobs.${tomlKey(name)}.params.${tomlKey(pname)}]`);
    lines.push(`type = ${tomlString(p.type)}`);
    if (p.required) lines.push("required = true");
    const def = p.default.trim() !== "" ? literal(p.type, p.default) : null;
    if (def !== null && !p.required) lines.push(`default = ${def}`);
    if (p.help.trim() !== "") lines.push(`help = ${tomlString(p.help.trim())}`);
    const choices = splitChoices(p.choices)
      .map((c) => literal(p.type, c))
      .filter((c): c is string => c !== null);
    if (choices.length > 0) lines.push(`choices = [${choices.join(", ")}]`);
  }
  return lines.join("\n") + "\n";
}

/** Whether the draft would produce a block the next deploy accepts, as far as this page can tell. */
export function draftProblems(d: JobDraft): string[] {
  const out: string[] = [];
  const n = jobNameProblem(d.name.trim());
  if (n) out.push(n);
  if (d.mode === "command" && d.command.trim() === "") out.push("A job needs a command.");
  if (d.mode === "args" && d.args.trim() === "") out.push("A job needs at least one argument.");
  const t = timeoutProblem(d.timeout);
  if (t) out.push(t);
  const m = memoryProblem(d.memory);
  if (m) out.push(m);
  for (const p of d.params) {
    const pp = paramProblem(p, d.params);
    if (pp) out.push(pp);
  }
  return out;
}
