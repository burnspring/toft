import { describe, expect, it } from "vitest";
import type { JobParamSpec } from "./api";
import {
  coerceParam, collectParams, initialParamDraft, jobRunCommand, orderedParams, shellQuote,
} from "./job-params";

const spec: Record<string, JobParamSpec> = {
  region: { type: "string", default: "emea", choices: ["emea", "amer", "apac"], help: "Which region" },
  days: { type: "int", required: true },
  ratio: { type: "float", default: 0.5 },
  dry_run: { type: "bool", default: true },
};

describe("the Run now form", () => {
  it("puts required params first", () => {
    expect(orderedParams(spec).map(([n]) => n)).toEqual(["days", "dry_run", "ratio", "region"]);
  });

  it("pre-fills every default", () => {
    expect(initialParamDraft(spec)).toEqual({ days: "", dry_run: true, ratio: "0.5", region: "emea" });
  });

  it("checks types, choices and required before submit", () => {
    expect(coerceParam("days", spec.days, "")).toEqual({ error: "days is required." });
    expect(coerceParam("days", spec.days, "1.5").error).toMatch(/whole number/);
    expect(coerceParam("days", spec.days, " 7 ")).toEqual({ value: 7 });
    expect(coerceParam("ratio", spec.ratio, "abc").error).toMatch(/a number/);
    expect(coerceParam("ratio", spec.ratio, "")).toEqual({});
    expect(coerceParam("region", spec.region, "mars").error).toMatch(/one of emea, amer, apac/);
    expect(coerceParam("dry_run", spec.dry_run, false)).toEqual({ value: false });
  });

  it("collects the params to send and the refusals by field", () => {
    const { params, errors } = collectParams(spec, { days: "3", dry_run: false, ratio: "", region: "amer" });
    expect(errors).toEqual({});
    expect(params).toEqual({ days: 3, dry_run: false, region: "amer" });
    expect(collectParams(spec, initialParamDraft(spec)).errors).toEqual({ days: "days is required." });
  });
});

describe("the equivalent CLI command", () => {
  it("writes only what differs from the defaults", () => {
    expect(
      jobRunCommand("nightly-report", { spec, params: { region: "amer", days: 3, dry_run: true } }),
    ).toBe("toft job run nightly-report --param region=amer --param days=3");
  });

  it("names the app and the instant, and quotes what a shell would split", () => {
    expect(
      jobRunCommand("report", {
        app: "sales",
        params: { title: "Q3 review's" },
        scheduledFor: "2026-10-06T03:00:00.000Z",
      }),
    ).toBe(`toft job run report --app sales --param 'title=Q3 review'\\''s' --for 2026-10-06T03:00:00.000Z`);
  });

  it("quotes an empty word", () => {
    expect(shellQuote("")).toBe("''");
  });
});
