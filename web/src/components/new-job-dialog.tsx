import { useState } from "react";
import { Plus, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { FieldError, FieldHelp } from "@/components/field-help";
import { TimeZoneField } from "@/components/time-zone-field";
import { CopyableText } from "@/components/copyable-text";
import { Code } from "@/components/code";
import { TableCard } from "@/components/table-card";
import type { JobParamType } from "@/lib/api";
import { describeCron } from "@/lib/cron-describe";
import {
  EMPTY_DRAFT, draftProblems, emptyParamRow, generateJobToml, jobNameProblem, memoryProblem,
  paramProblem, timeoutProblem, type JobDraft, type ParamRow,
} from "@/lib/job-toml";
import { copyToClipboard } from "@/lib/clipboard";
import { fieldErrorId } from "@/lib/utils";

const MODE_LABEL: Record<JobDraft["mode"], string> = {
  command: "A command line",
  args: "Arguments to the app’s binary",
};
/** A select item cannot carry an empty value, so "leave it out" has a word. */
const DEFAULT = "default";
const OVERLAP_LABEL: Record<string, string> = {
  [DEFAULT]: "Wait for it (default)",
  skip: "Skip this firing",
};
const NOTIFY_LABEL: Record<string, string> = {
  [DEFAULT]: "Nobody (default)",
  failures: "When a run fails",
  always: "After every run",
};
const TYPE_LABEL: Record<JobParamType, string> = {
  string: "Text",
  int: "Whole number",
  float: "Number",
  bool: "On or off",
};

/**
 * The New job helper. Jobs are declared in toft.toml and nowhere else, so this
 * saves nothing: it writes the `[jobs.<name>]` block to paste into the file,
 * and the deploy that makes it real.
 *
 * The checks mirror the manifest parser so the block it writes is one the next
 * deploy accepts — except the install's own ceilings on timeout and memory,
 * which are applied at deploy (a value above them is lowered and says so).
 */
export function NewJobDialog({
  appSlug,
  open,
  onOpenChange,
}: {
  appSlug: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const [d, setD] = useState<JobDraft>(EMPTY_DRAFT);
  const set = <K extends keyof JobDraft>(k: K, v: JobDraft[K]) => setD((x) => ({ ...x, [k]: v }));
  const setParam = (i: number, row: Partial<ParamRow>) =>
    setD((x) => ({ ...x, params: x.params.map((p, j) => (j === i ? { ...p, ...row } : p)) }));

  const nameProblem = d.name === "" ? null : jobNameProblem(d.name.trim());
  const tProblem = timeoutProblem(d.timeout);
  const mProblem = memoryProblem(d.memory);
  const schedule = d.schedule.trim();
  const described = schedule ? describeCron(schedule) : null;
  const scheduleFields = schedule.split(/\s+/).length;
  const toml = generateJobToml(d);
  const problems = draftProblems(d);
  const deployCommand = `toft deploy --app ${appSlug}`;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent scrollable className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>New job</DialogTitle>
          <DialogDescription>
            Jobs are declared in <Code>toft.toml</Code>. Fill this in, paste the block into the
            file and deploy. Nothing is saved here.
          </DialogDescription>
        </DialogHeader>
        <DialogBody className="space-y-5">
          <div className="space-y-2">
            <Label htmlFor="job-name">Name</Label>
            <Input
              id="job-name" value={d.name} placeholder="nightly-report" className="font-mono"
              aria-invalid={!!nameProblem}
              aria-describedby={nameProblem ? fieldErrorId("job-name") : undefined}
              onChange={(e) => set("name", e.target.value)}
            />
            {nameProblem ? (
              <FieldError field="job-name">{nameProblem}</FieldError>
            ) : (
              <FieldHelp>Lowercase letters, digits and dashes.</FieldHelp>
            )}
          </div>

          <div className="space-y-2">
            <Label htmlFor="job-mode">What runs</Label>
            <Select value={d.mode} onValueChange={(v) => v && set("mode", v as JobDraft["mode"])}>
              <SelectTrigger id="job-mode" className="w-full">
                <SelectValue>{(v) => (v ? MODE_LABEL[v as JobDraft["mode"]] : "Choose…")}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="command">{MODE_LABEL.command}</SelectItem>
                <SelectItem value="args">{MODE_LABEL.args}</SelectItem>
              </SelectContent>
            </Select>
            {d.mode === "command" ? (
              <>
                <Input
                  id="job-command" aria-label="Command" value={d.command} className="font-mono"
                  placeholder="python -m reports.nightly"
                  onChange={(e) => set("command", e.target.value)}
                />
                <FieldHelp>Runs in the app’s directory. Params arrive as environment variables, never in the command.</FieldHelp>
              </>
            ) : (
              <>
                <Textarea
                  id="job-args" aria-label="Arguments, one per line" value={d.args} rows={3}
                  className="font-mono" placeholder={"job\nnightly-report"}
                  onChange={(e) => set("args", e.target.value)}
                />
                <FieldHelp>One argument per line, passed to the app’s single binary — a Go app’s.</FieldHelp>
              </>
            )}
          </div>

          <div className="space-y-2">
            <Label htmlFor="job-schedule">Schedule</Label>
            <Input
              id="job-schedule" value={d.schedule} placeholder="0 3 * * *" className="font-mono"
              onChange={(e) => set("schedule", e.target.value)}
            />
            <FieldHelp>
              {schedule === ""
                ? "Empty runs only when started — by the app, Run now or the CLI."
                : described
                  ? `${described}${d.timezone.trim() && d.timezone.trim() !== "UTC" ? `, ${d.timezone.trim()} time` : " UTC"}.`
                  : scheduleFields === 5 || schedule.startsWith("@")
                    ? "A schedule this page can’t put into words. The deploy checks it."
                    : "A cron schedule has five fields: minute, hour, day of month, month, day of week."}
            </FieldHelp>
          </div>

          {schedule !== "" && (
            <div className="grid gap-4 sm:grid-cols-2">
              <TimeZoneField id="job-zone" value={d.timezone} onChange={(z) => set("timezone", z)} />
              <div className="space-y-2">
                <Label htmlFor="job-overlap">If the last run is still going</Label>
                <Select
                  value={d.overlap || DEFAULT}
                  onValueChange={(v) => set("overlap", (v === DEFAULT || !v ? "" : v) as JobDraft["overlap"])}
                >
                  <SelectTrigger id="job-overlap" className="w-full">
                    <SelectValue>{(v) => (v ? OVERLAP_LABEL[v as string] : "Choose…")}</SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value={DEFAULT}>{OVERLAP_LABEL[DEFAULT]}</SelectItem>
                    <SelectItem value="skip">{OVERLAP_LABEL.skip}</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>
          )}

          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-2">
              <Label htmlFor="job-timeout">Timeout</Label>
              <Input
                id="job-timeout" value={d.timeout} placeholder="45m" className="font-mono"
                aria-invalid={!!tProblem}
                aria-describedby={tProblem ? fieldErrorId("job-timeout") : undefined}
                onChange={(e) => set("timeout", e.target.value)}
              />
              {tProblem ? (
                <FieldError field="job-timeout">{tProblem}</FieldError>
              ) : (
                <FieldHelp>Blank uses the install default.</FieldHelp>
              )}
            </div>
            <div className="space-y-2">
              <Label htmlFor="job-memory">Memory (MB)</Label>
              <Input
                id="job-memory" value={d.memory} placeholder="1024" inputMode="numeric"
                aria-invalid={!!mProblem}
                aria-describedby={mProblem ? fieldErrorId("job-memory") : undefined}
                onChange={(e) => set("memory", e.target.value)}
              />
              {mProblem ? (
                <FieldError field="job-memory">{mProblem}</FieldError>
              ) : (
                <FieldHelp>Blank uses the install default.</FieldHelp>
              )}
            </div>
          </div>

          <div className="space-y-2">
            <Label htmlFor="job-notify">Email the app’s owners</Label>
            <Select
              value={d.notify || DEFAULT}
              onValueChange={(v) => set("notify", (v === DEFAULT || !v ? "" : v) as JobDraft["notify"])}
            >
              <SelectTrigger id="job-notify" className="w-full">
                <SelectValue>{(v) => (v ? NOTIFY_LABEL[v as string] : "Choose…")}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={DEFAULT}>{NOTIFY_LABEL[DEFAULT]}</SelectItem>
                <SelectItem value="failures">{NOTIFY_LABEL.failures}</SelectItem>
                <SelectItem value="always">{NOTIFY_LABEL.always}</SelectItem>
              </SelectContent>
            </Select>
          </div>

          <div className="space-y-2">
            <div className="flex items-center justify-between gap-2">
              <span className="text-sm font-medium">Params</span>
              <Button
                type="button" variant="outline" size="sm"
                onClick={() => setD((x) => ({ ...x, params: [...x.params, emptyParamRow()] }))}
              >
                <Plus className="mr-1.5 h-3.5 w-3.5" />
                Add param
              </Button>
            </div>
            {d.params.length === 0 ? (
              <FieldHelp>None. A param is a value chosen when a run starts.</FieldHelp>
            ) : (
              d.params.map((p, i) => (
                <ParamRowEditor
                  key={i}
                  index={i}
                  row={p}
                  problem={p.name === "" ? null : paramProblem(p, d.params)}
                  onChange={(row) => setParam(i, row)}
                  onRemove={() => setD((x) => ({ ...x, params: x.params.filter((_, j) => j !== i) }))}
                />
              ))
            )}
          </div>

          <div className="space-y-2">
            <span className="text-sm font-medium">Add this to toft.toml</span>
            <CopyableText text={toml} subject="toft.toml block" />
            {problems.length > 0 ? (
              <FieldHelp tone="attention">{problems[0]}</FieldHelp>
            ) : null}
          </div>
          <div className="space-y-2">
            <span className="text-sm font-medium">Then deploy</span>
            <CopyableText text={deployCommand} subject="command" />
          </div>
        </DialogBody>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
            Close
          </Button>
          <Button type="button" onClick={() => void copyToClipboard(toml, "toft.toml block")}>
            Copy the block
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function ParamRowEditor({
  index, row, problem, onChange, onRemove,
}: {
  index: number;
  row: ParamRow;
  problem: string | null;
  onChange: (row: Partial<ParamRow>) => void;
  onRemove: () => void;
}) {
  const n = index + 1;
  const errId = `param-row-${index}`;
  return (
    <TableCard flat className="space-y-2 p-3">
      <div className="flex flex-wrap items-center gap-2">
        <Input
          aria-label={`Param ${n} name`} value={row.name} placeholder="region"
          className="min-w-0 flex-1 font-mono"
          aria-invalid={!!problem} aria-describedby={problem ? fieldErrorId(errId) : undefined}
          onChange={(e) => onChange({ name: e.target.value })}
        />
        <Select value={row.type} onValueChange={(v) => v && onChange({ type: v as JobParamType })}>
          <SelectTrigger className="w-36" aria-label={`Param ${n} type`}>
            <SelectValue>{(v) => (v ? TYPE_LABEL[v as JobParamType] : "Type")}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            {(Object.keys(TYPE_LABEL) as JobParamType[]).map((t) => (
              <SelectItem key={t} value={t}>{TYPE_LABEL[t]}</SelectItem>
            ))}
          </SelectContent>
        </Select>
        <span className="flex items-center gap-2 text-xs">
          <Switch
            size="sm" checked={row.required} aria-label={`Param ${n} required`}
            onCheckedChange={(v: boolean) => onChange({ required: v })}
          />
          Required
        </span>
        <Button type="button" variant="ghost" size="icon-sm" aria-label={`Remove param ${n}`} onClick={onRemove}>
          <X className="h-4 w-4" />
        </Button>
      </div>
      <div className="grid gap-2 sm:grid-cols-2">
        <Input
          aria-label={`Param ${n} default`} value={row.default} disabled={row.required}
          placeholder={row.required ? "No default when required" : "Default"}
          onChange={(e) => onChange({ default: e.target.value })}
        />
        <Input
          aria-label={`Param ${n} choices, comma-separated`} value={row.choices}
          placeholder="Choices, comma-separated"
          onChange={(e) => onChange({ choices: e.target.value })}
        />
      </div>
      <Input
        aria-label={`Param ${n} help`} value={row.help} placeholder="Help shown beside the field"
        onChange={(e) => onChange({ help: e.target.value })}
      />
      {problem && <FieldError field={errId}>{problem}</FieldError>}
    </TableCard>
  );
}
