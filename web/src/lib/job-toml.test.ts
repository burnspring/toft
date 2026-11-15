import { describe, expect, it } from "vitest";
import {
  EMPTY_DRAFT, draftProblems, emptyParamRow, generateJobToml, jobNameProblem, memoryProblem,
  paramProblem, timeoutProblem, timeoutSeconds, type JobDraft,
} from "./job-toml";

const draft = (over: Partial<JobDraft>): JobDraft => ({ ...EMPTY_DRAFT, ...over });

describe("generateJobToml", () => {
  it("writes the smallest job", () => {
    expect(generateJobToml(draft({ name: "cleanup", command: "python -m cleanup" }))).toBe(
      '[jobs.cleanup]\ncommand = "python -m cleanup"\n',
    );
  });

  it("writes every key, in the documented order", () => {
    const out = generateJobToml(
      draft({
        name: "nightly-report",
        command: "python -m reports.nightly",
        timeout: "45m",
        memory: "1024",
        schedule: "0  3 * * *",
        timezone: "Europe/London",
        overlap: "skip",
        notify: "failures",
        params: [
          {
            name: "region", type: "string", required: false, default: "emea",
            choices: "emea, amer, apac", help: "Which region to report on",
          },
          { name: "days", type: "int", required: true, default: "", choices: "", help: "" },
          { name: "ratio", type: "float", required: false, default: "1", choices: "", help: "" },
          { name: "dry_run", type: "bool", required: false, default: "true", choices: "", help: "" },
        ],
      }),
    );
    expect(out).toBe(
      [
        "[jobs.nightly-report]",
        'command = "python -m reports.nightly"',
        'timeout = "45m"',
        "memory = 1024",
        'schedule = "0 3 * * *"',
        'timezone = "Europe/London"',
        'overlap = "skip"',
        'notify = "failures"',
        "",
        "[jobs.nightly-report.params.region]",
        'type = "string"',
        'default = "emea"',
        'help = "Which region to report on"',
        'choices = ["emea", "amer", "apac"]',
        "",
        "[jobs.nightly-report.params.days]",
        'type = "int"',
        "required = true",
        "",
        "[jobs.nightly-report.params.ratio]",
        'type = "float"',
        "default = 1.0",
        "",
        "[jobs.nightly-report.params.dry_run]",
        'type = "bool"',
        "default = true",
        "",
      ].join("\n"),
    );
  });

  it("writes args as an array, and a timeout in seconds as a number", () => {
    expect(
      generateJobToml(draft({ name: "sync", mode: "args", args: "job\n\n sync \n", timeout: "600" })),
    ).toBe('[jobs.sync]\nargs = ["job", "sync"]\ntimeout = 600\n');
  });

  it("leaves out a zone and an overlap with no schedule, and UTC always", () => {
    expect(
      generateJobToml(draft({ name: "a", command: "x", timezone: "Europe/Paris", overlap: "queue" })),
    ).toBe('[jobs.a]\ncommand = "x"\n');
    expect(generateJobToml(draft({ name: "a", command: "x", schedule: "0 1 * * *", timezone: "UTC" }))).toBe(
      '[jobs.a]\ncommand = "x"\nschedule = "0 1 * * *"\n',
    );
  });

  it("escapes what a TOML string cannot hold bare", () => {
    expect(generateJobToml(draft({ name: "a", command: 'echo "hi" \\ there' }))).toBe(
      '[jobs.a]\ncommand = "echo \\"hi\\" \\\\ there"\n',
    );
  });
});

describe("the helper's checks mirror the manifest's", () => {
  it("names", () => {
    expect(jobNameProblem("nightly-report")).toBeNull();
    expect(jobNameProblem("")).not.toBeNull();
    expect(jobNameProblem("Nightly")).not.toBeNull();
    expect(jobNameProblem("-x")).not.toBeNull();
    expect(jobNameProblem("x".repeat(61))).not.toBeNull();
  });

  it("timeouts", () => {
    expect(timeoutSeconds("1h30m")).toBe(5400);
    expect(timeoutSeconds("90")).toBe(90);
    expect(timeoutSeconds("2d")).toBeNull();
    expect(timeoutProblem("")).toBeNull();
    expect(timeoutProblem("8000000")).toMatch(/7 days/);
    expect(timeoutProblem("soon")).toMatch(/duration/);
  });

  it("memory", () => {
    expect(memoryProblem("1024")).toBeNull();
    expect(memoryProblem("32")).toMatch(/64/);
    expect(memoryProblem("1.5")).toMatch(/whole/);
  });

  it("params", () => {
    const row = { ...emptyParamRow(), name: "region" };
    expect(paramProblem(row, [row])).toBeNull();
    expect(paramProblem({ ...row, required: true, default: "x" }, [])).toMatch(/required/);
    expect(paramProblem({ ...row, type: "int", default: "x" }, [])).toMatch(/whole number/);
    expect(paramProblem({ ...row, choices: "a,b", default: "c" }, [])).toMatch(/choices/);
    expect(paramProblem(row, [row, { ...row }])).toMatch(/twice/);
  });

  it("collects the draft's problems", () => {
    expect(draftProblems(draft({ name: "a", command: "x" }))).toEqual([]);
    expect(draftProblems(draft({ name: "", command: "" }))).toHaveLength(2);
  });
});
