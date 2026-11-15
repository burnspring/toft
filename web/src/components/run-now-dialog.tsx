import { useState } from "react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { FieldError, FieldHelp } from "@/components/field-help";
import { TimeZoneField } from "@/components/time-zone-field";
import { Disclosure } from "@/components/disclosure";
import { Code } from "@/components/code";
import { api, type JobListEntry, type JobParamSpec } from "@/lib/api";
import { splitSchedule, timeZoneValid, zonedWallToInstant } from "@/lib/schedule";
import {
  collectParams, initialParamDraft, jobRunCommand, orderedParams, type ParamDraft,
} from "@/lib/job-params";
import { refusalText } from "@/lib/automation";
import { CopyableText } from "@/components/copyable-text";
import { fieldErrorId } from "@/lib/utils";

/**
 * Run now: a form built from the job's declared params (toft.toml is the
 * contract), and the `toft job run` line that does the same thing.
 *
 * Each param gets the input its type asks for, its default filled in, its
 * choices as a dropdown and its help beside it. Required fields are checked
 * here before the request, in the words the install would refuse with.
 *
 * "Run for" names the instant the run is for — a re-run of Tuesday's work on
 * Thursday (ADR §104). The run sees it as TOFT_SCHEDULED_FOR; left empty, the
 * run is for nothing in particular, never "now".
 */
export function RunNowDialog({
  appId,
  appSlug,
  job,
  open,
  onOpenChange,
  onStarted,
}: {
  appId: string;
  appSlug: string;
  job: JobListEntry;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onStarted: () => void;
}) {
  const spec = job.params_spec;
  const fields = orderedParams(spec);
  const [draft, setDraft] = useState<ParamDraft>(() => initialParamDraft(spec));
  const [submitted, setSubmitted] = useState(false);
  const [wall, setWall] = useState("");
  const [zone, setZone] = useState(splitSchedule(job.schedule).zone);
  const [saving, setSaving] = useState(false);

  const { params, errors } = collectParams(spec, draft);
  const instant = wall ? zonedWallToInstant(wall, zone) : null;
  const wallValid = wall === "" || instant !== null;
  const zoneValid = wall === "" || timeZoneValid(zone);
  const ready = Object.keys(errors).length === 0 && wallValid && zoneValid;

  const command = jobRunCommand(job.name, {
    app: appSlug,
    spec,
    params,
    scheduledFor: instant ? instant.toISOString() : undefined,
  });

  async function handleRun() {
    setSubmitted(true);
    if (!ready) return;
    setSaving(true);
    try {
      const run = await api.startJobRun(appId, job.name, {
        params,
        scheduled_for: instant ? instant.toISOString() : undefined,
      });
      toast.success(
        run.deduplicated
          ? `“${job.name}” is already queued with these params`
          : `“${job.name}” queued`,
      );
      onStarted();
      onOpenChange(false);
    } catch (err) {
      toast.error(refusalText(err, `Couldn’t start “${job.name}”`));
    } finally {
      setSaving(false);
    }
  }

  const set = (name: string, v: string | boolean) => setDraft((d) => ({ ...d, [name]: v }));

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent scrollable className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Run {job.name}</DialogTitle>
          <DialogDescription>
            {fields.length === 0
              ? "This job declares no params. It runs with its current release."
              : "Params come from toft.toml. Required ones are marked."}
          </DialogDescription>
        </DialogHeader>
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault();
            if (!saving) void handleRun();
          }}
        >
          <DialogBody className="space-y-4">
            {fields.map(([name, p]) => (
              <ParamField
                key={name}
                id={`param-${name}`}
                name={name}
                spec={p}
                value={draft[name]}
                error={submitted ? errors[name] : undefined}
                onChange={(v) => set(name, v)}
              />
            ))}

            <Disclosure label="Run for a past date" size="sm" defaultOpen={false}>
              <div className="space-y-4 pt-2">
                <div className="space-y-2">
                  <Label htmlFor="run-for-time">Run as of</Label>
                  <Input
                    id="run-for-time" type="datetime-local" value={wall}
                    aria-invalid={!wallValid}
                    aria-describedby={wallValid ? undefined : fieldErrorId("run-for-time")}
                    onChange={(e) => setWall(e.target.value)}
                  />
                  {wallValid ? (
                    <FieldHelp>The run reads this instant as <Code>TOFT_SCHEDULED_FOR</Code>. Leave empty for none.</FieldHelp>
                  ) : (
                    <FieldError field="run-for-time">Not a date and time.</FieldError>
                  )}
                </div>
                <TimeZoneField id="run-for-zone" value={zone} onChange={setZone} />
              </div>
            </Disclosure>

            <div className="space-y-1.5">
              <span className="text-xs font-medium text-muted-foreground">The same from a terminal</span>
              <CopyableText text={command} subject="command" />
            </div>
          </DialogBody>
          <DialogFooter>
            <Button type="button" variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={saving || (submitted && !ready)}>
              {saving ? "Starting…" : "Run"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/** One declared param, as the input its type asks for. */
function ParamField({
  id, name, spec, value, error, onChange,
}: {
  id: string;
  name: string;
  spec: JobParamSpec;
  value: string | boolean | undefined;
  error?: string;
  onChange: (v: string | boolean) => void;
}) {
  const describedBy = error ? fieldErrorId(id) : undefined;
  const label = (
    <Label htmlFor={id} className="font-mono">
      {name}
      {spec.required && <span className="ml-1.5 font-sans text-xs font-normal text-muted-foreground">Required</span>}
    </Label>
  );

  if (spec.type === "bool") {
    return (
      <div className="space-y-1">
        <div className="flex items-center justify-between gap-4">
          {label}
          <Switch id={id} checked={value === true} onCheckedChange={(v: boolean) => onChange(v)} />
        </div>
        {spec.help && <FieldHelp>{spec.help}</FieldHelp>}
      </div>
    );
  }

  const text = typeof value === "string" ? value : "";
  const choices = (spec.choices ?? []).map(String);

  return (
    <div className="space-y-2">
      {label}
      {choices.length > 0 ? (
        <Select value={text === "" ? null : text} onValueChange={(v) => onChange(v ?? "")}>
          <SelectTrigger id={id} className="w-full" aria-invalid={!!error} aria-describedby={describedBy}>
            <SelectValue>{(v) => (v ? String(v) : "Choose…")}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            {choices.map((c) => (
              <SelectItem key={c} value={c}>{c}</SelectItem>
            ))}
          </SelectContent>
        </Select>
      ) : (
        <Input
          id={id}
          type={spec.type === "int" || spec.type === "float" ? "number" : "text"}
          step={spec.type === "float" ? "any" : spec.type === "int" ? "1" : undefined}
          inputMode={spec.type === "int" ? "numeric" : spec.type === "float" ? "decimal" : undefined}
          value={text}
          aria-invalid={!!error}
          aria-describedby={describedBy}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
      {error ? (
        <FieldError field={id}>{error}</FieldError>
      ) : (
        <FieldHelp>
          {[spec.help, typeLabel(spec)].filter(Boolean).join(" · ")}
        </FieldHelp>
      )}
    </div>
  );
}

function typeLabel(spec: JobParamSpec): string {
  const t = { string: "Text", int: "Whole number", float: "Number", bool: "On or off" }[spec.type] ?? spec.type;
  return spec.default !== undefined && spec.default !== null ? `${t}, default ${String(spec.default)}` : t;
}
