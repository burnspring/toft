import { toneText } from "@/lib/tone";
import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import {
  api,
  type TelemetrySettings,
  type UpdateTelemetrySettings,
} from "@/lib/api";
import { LicenseLock } from "@/components/license-lock";
import { PageHeader } from "@/components/page-header";
import { useHashScroll } from "@/hooks/use-hash-scroll";
import { QueryError } from "@/components/query-error";
import { Section } from "@/components/section";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { RelativeTime } from "@/components/relative-time";
import { PendingBar } from "@/components/pending-bar";
import { useUnloadGuard } from "@/hooks/use-unload-guard";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { Notice } from "@/components/notice";
import { UnreadableCredentialNotice } from "@/components/unreadable-credential";
import { FieldError, FieldHelp } from "@/components/field-help";
import { fieldErrorId } from "@/lib/utils";
import { FieldRow } from "@/components/field-row";
import { credentialPlaceholder } from "@/lib/credentials";

const TEMPORALITY_LABEL: Record<TelemetrySettings["temporality"], string> = {
  cumulative: "Cumulative — Prometheus, Grafana, most backends",
  delta: "Delta — Datadog and similar",
};

/**
 * Admin → Telemetry.
 *
 * Its own page rather than a card on Settings, for the two reasons Email has
 * one: it holds a credential, and it has an action — the test export — that
 * talks to something outside this install and can fail in ways only that
 * thing's own error text explains.
 *
 * Saving applies immediately. The platform rebuilds its export pipeline in
 * place, so an administrator who fixes a collector address can watch the
 * failure count stop climbing rather than go and find somebody who can restart
 * the platform.
 */
const PAGE_DESCRIPTION = "This install’s own traces and metrics. Changes apply immediately.";

export function AdminTelemetryPage() {
  const queryClient = useQueryClient();
  const { data: settings, isLoading, error, refetch } = useQuery({
    queryKey: ["telemetry-settings"],
    queryFn: api.getTelemetrySettings,
    // The health counters are the reason to come back to this page, so it
    // refreshes on its own — an administrator watching a broken export recover
    // should not have to reload.
    refetchInterval: 15_000,
    retry: false,
  });
  // ⌘K reaches a setting here as `/admin/telemetry#export`; the card it names
  // exists only once the document has loaded.
  useHashScroll(!!settings);

  if (error) {
    return (
      <>
        <PageHeader title="Telemetry" description={PAGE_DESCRIPTION} />
        <QueryError
          title="The telemetry settings could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </>
    );
  }

  if (isLoading || !settings) {
    return (
      <>
        <PageHeader title="Telemetry" description={PAGE_DESCRIPTION} />
        <div className="space-y-6">
          <Skeleton className="h-40 w-full rounded-lg" />
          <Skeleton className="h-64 w-full rounded-lg" />
        </div>
      </>
    );
  }

  return (
    <>
      <PageHeader title="Telemetry" description={PAGE_DESCRIPTION} />

      <LicenseLock feature="telemetry" />

      {/* Keyed on what the server stored, so a save re-seeds the form from the
          saved document rather than leaving edited state to drift from it. */}
      {/* The measure both settings surfaces use; uncapped, inputs ran ~1,125px. */}
      <div className="max-w-3xl">
        <TelemetryForm
          key={`${settings.configured}:${settings.endpoint}:${settings.headers_set}:${settings.headers_unreadable}`}
          settings={settings}
          onSaved={(saved) => {
            queryClient.setQueryData(["telemetry-settings"], saved);
            queryClient.invalidateQueries({ queryKey: ["telemetry-settings"] });
          }}
        />
      </div>
    </>
  );
}

function TelemetryForm({
  settings,
  onSaved,
}: {
  settings: TelemetrySettings;
  onSaved: (saved: TelemetrySettings) => void;
}) {
  const [enabled, setEnabled] = useState(settings.enabled);
  const [endpoint, setEndpoint] = useState(settings.endpoint);
  // Never seeded from the server: the credential does not come back as a value,
  // and an input showing dots it did not receive is a lie.
  const [headers, setHeaders] = useState("");
  const [traces, setTraces] = useState(settings.traces);
  const [metrics, setMetrics] = useState(settings.metrics);
  const [sampleRatio, setSampleRatio] = useState(String(Math.round(settings.sample_ratio * 100)));
  const [temporality, setTemporality] = useState<TelemetrySettings["temporality"]>(
    settings.temporality,
  );

  const [saving, setSaving] = useState(false);
  const [handBack, setHandBack] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testError, setTestError] = useState("");

  const savedRatio = String(Math.round(settings.sample_ratio * 100));
  /** Every field that differs from what is saved — the pending bar's count. */
  const changed = [
    enabled !== settings.enabled,
    endpoint !== settings.endpoint,
    headers !== "",
    traces !== settings.traces,
    metrics !== settings.metrics,
    sampleRatio !== savedRatio,
    temporality !== settings.temporality,
  ].filter(Boolean).length;
  const dirty = changed > 0;
  useUnloadGuard(dirty, "Your telemetry changes haven’t been saved.");

  function discard() {
    setEnabled(settings.enabled);
    setEndpoint(settings.endpoint);
    setHeaders("");
    setTraces(settings.traces);
    setMetrics(settings.metrics);
    setSampleRatio(savedRatio);
    setTemporality(settings.temporality);
  }

  // The empty string is not zero per cent. `Number("")` is 0, so a cleared
  // field read as a valid nought and Save sent it — an administrator who
  // selected the number to retype it and then pressed Save turned tracing off
  // and was told the settings had been saved.
  const ratioValid = (() => {
    const raw = sampleRatio.trim();
    if (raw === "") return false;
    const n = Number(raw);
    return Number.isFinite(n) && n >= 0 && n <= 100;
  })();

  async function save(useEnvironment = false) {
    // Read before the response replaces `settings`: the toast has to name what
    // went, and by the time it fires the credential is already gone.
    const hadCredential = settings.headers_set;
    setSaving(true);
    setTestError("");
    const payload: UpdateTelemetrySettings = {
      enabled,
      endpoint: endpoint.trim(),
      traces,
      metrics,
      sample_ratio: Number(sampleRatio) / 100,
      temporality,
    };
    if (useEnvironment) payload.use_environment = true;
    // An empty credential means "keep the stored one" on the way through the
    // API, so it is omitted rather than sent blank.
    if (headers.trim()) payload.headers = headers.trim();
    try {
      const res = await api.updateTelemetrySettings(payload);
      onSaved(res.settings);
      setHeaders("");
      if (res.warning) {
        // A warning, not an error: the save landed. Reporting it in red made
        // an administrator go looking for the change that had not been made.
        toast.warning(res.warning, { duration: 10_000 });
      } else if (useEnvironment) {
        toast.success("Now following the OTEL_* environment", {
          description: hadCredential
            ? "The saved endpoint and credential were removed."
            : "The saved endpoint was removed.",
        });
      } else if (!enabled) {
        toast.success("Saved — export is off");
      } else {
        toast.success("Saved");
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t save the telemetry settings");
    } finally {
      setSaving(false);
    }
  }

  async function test() {
    setTesting(true);
    setTestError("");
    try {
      const res = await api.testTelemetry();
      if (res.ok) {
        toast.success("The collector accepted the export");
      } else {
        setTestError(res.error ?? "The collector refused the export");
        toast.error("The collector refused the export");
      }
    } catch (err) {
      const message = err instanceof Error ? err.message : "The test export failed";
      setTestError(message);
      toast.error(message);
    } finally {
      setTesting(false);
    }
  }

  const health = settings.health;

  return (
    // Room under the last section for the fixed pending bar, so it never sits
    // over the scrape endpoint while there is something to save.
    <div className={dirty ? "space-y-6 pb-20" : "space-y-6"}>
      {!settings.configured && (
        <Notice tone="neutral" size="md" title="This install follows its environment">
          The <code className="font-mono text-xs">OTEL_*</code> environment variables decide until
          you save here. You can hand it back at any time.
        </Notice>
      )}

      {settings.configured && settings.env_present && (
        <Notice tone="neutral" size="md" title="These settings override the environment">
          <code className="font-mono text-xs">OTEL_EXPORTER_OTLP_ENDPOINT</code> is set on this
          host to <span className="font-mono">{settings.env_endpoint}</span>, but this page wins.
        </Notice>
      )}

      <Section
        id="export"
        title="Where to export"
        description="An OpenTelemetry collector that speaks OTLP over HTTP. Save before testing."
      >
        <div className="divide-y divide-border/60 px-4 [&>*]:py-4">
          <FieldRow
            label="Export telemetry"
            htmlFor="telemetry-enabled"
            help="When off, nothing is exported and Toft works as before."
          >
            <Switch
              id="telemetry-enabled"
              checked={enabled}
              onCheckedChange={setEnabled}
            />
          </FieldRow>

          <FieldRow
            label="Collector endpoint"
            htmlFor="telemetry-endpoint"
            help={
              <>
                The base URL. Toft appends <code className="font-mono">/v1/traces</code> and{" "}
                <code className="font-mono">/v1/metrics</code>. HTTP only, not gRPC.
              </>
            }
          >
            <Input
              id="telemetry-endpoint"
              value={endpoint}
              onChange={(e) => setEndpoint(e.target.value)}
              placeholder="http://collector.internal:4318"
              autoComplete="off"
            />
          </FieldRow>

          <FieldRow
            label="Credential"
            htmlFor="telemetry-headers"
            extra={
              settings.headers_unreadable ? (
                <UnreadableCredentialNotice what="collector credential" />
              ) : (
                <FieldHelp>
                  Headers sent with every export, as{" "}
                  <code className="font-mono">key=value,key=value</code>. Leave blank to keep the
                  stored one.
                </FieldHelp>
              )
            }
          >
            <Input
              id="telemetry-headers"
              type="password"
              value={headers}
              onChange={(e) => setHeaders(e.target.value)}
              placeholder={credentialPlaceholder(
                settings.headers_set,
                settings.headers_unreadable,
                "authorization=Bearer …",
              )}
              autoComplete="off"
            />
          </FieldRow>
        </div>
      {/* The foot of the card it tests — the endpoint and its credential —
          rather than a button floating between two cards. Save is the
          pending bar's. */}
      <div className="flex flex-wrap items-center gap-3 border-t border-border/60 px-4 py-3">
        {/* Not over unsaved edits. The test posts to whatever is *saved*, so
            an administrator who pasted a new endpoint and pressed it got the
            old collector's answer reported as the new one's — a green result
            about a configuration that is not in force. The reason is beside
            the button, never a `title` on it: a disabled control is not
            focusable and swallows the pointer. */}
        <Button
          variant="outline"
          onClick={test}
          disabled={testing || !settings.enabled || dirty}
        >
          {testing ? "Testing…" : "Test export"}
        </Button>
        {settings.configured && settings.env_present && (
          // Not reversible from this page. The endpoint survives in local state
          // until a reload, but the collector credential is write-only in the
          // UI (hard rule 6) — nothing can read it back, so an administrator
          // who throws this has to go and find it again. Plain confirm.
          <Button
            variant="ghost"
            onClick={() => setHandBack(true)}
            disabled={saving}
          >
            Follow the environment instead…
          </Button>
        )}
        {dirty ? (
          <span className="text-xs text-muted-foreground">Save before testing.</span>
        ) : (
          !settings.enabled && (
            <span className="text-xs text-muted-foreground">Turn on export and save to test it.</span>
          )
        )}
      </div>
      </Section>

      <Section
        id="signals"
        title="What to export"
      >
        <div className="divide-y divide-border/60 px-4 [&>*]:py-4">
          <FieldRow
            label="Traces"
            htmlFor="telemetry-traces"
            help="One span per request, per build phase, per job dispatch."
          >
            <Switch id="telemetry-traces" checked={traces} onCheckedChange={setTraces} />
          </FieldRow>

          <FieldRow
            label="Metrics"
            htmlFor="telemetry-metrics"
            help="Request rates and latency, the build pipeline, the workload inventory."
          >
            <Switch id="telemetry-metrics" checked={metrics} onCheckedChange={setMetrics} />
          </FieldRow>

          <FieldRow
            label="Trace sampling"
            htmlFor="telemetry-ratio"
            extra={
              ratioValid ? (
                <FieldHelp>Higher costs more storage in your backend and more CPU here.</FieldHelp>
              ) : (
                <FieldError field="telemetry-ratio">Enter a percentage between 0 and 100.</FieldError>
              )
            }
          >
            <div className="flex items-center gap-2">
              <Input
                id="telemetry-ratio"
                type="number"
                min={0}
                max={100}
                value={sampleRatio}
                onChange={(e) => setSampleRatio(e.target.value)}
                className="w-24"
                aria-invalid={!ratioValid}
                aria-describedby={ratioValid ? undefined : fieldErrorId("telemetry-ratio")}
              />
              <span className="text-sm text-muted-foreground">% of requests</span>
            </div>
          </FieldRow>

          <FieldRow
            label="Metric temporality"
            htmlFor="telemetry-temporality"
            help="Leave this alone unless your backend asked for delta."
          >
            <Select
              value={temporality}
              onValueChange={(v) => setTemporality(v as TelemetrySettings["temporality"])}
            >
              <SelectTrigger id="telemetry-temporality" className="w-full">
                <SelectValue className="min-w-0 truncate">
                  {(v) => (v ? TEMPORALITY_LABEL[v as TelemetrySettings["temporality"]] : "Choose…")}
                </SelectValue>
              </SelectTrigger>
              <SelectContent>
                {(Object.keys(TEMPORALITY_LABEL) as TelemetrySettings["temporality"][]).map(
                  (t) => (
                    <SelectItem key={t} value={t}>
                      {TEMPORALITY_LABEL[t]}
                    </SelectItem>
                  ),
                )}
              </SelectContent>
            </Select>
          </FieldRow>
        </div>
      </Section>

      <PendingBar
        count={changed}
        broken={ratioValid ? 0 : 1}
        saving={saving}
        onDiscard={discard}
        onSave={() => void save()}
      />

      {health && (
        <Section
          title="Export health"
          description={
            <>
              Since <RelativeTime iso={health.since} />. Resets when settings change or Toft
              restarts.
            </>
          }
        >
          <div className="space-y-4 p-4">
            <dl className="grid grid-cols-1 gap-3 text-sm sm:grid-cols-3">
              <div className="flex justify-between sm:flex-col sm:gap-0.5">
                <dt className="text-muted-foreground">Exported</dt>
                <dd className="tabular-nums">
                  {health.spans_exported.toLocaleString()} spans ·{" "}
                  {health.metric_exports.toLocaleString()} metric batches
                </dd>
              </div>
              <div className="flex justify-between sm:flex-col sm:gap-0.5">
                <dt className="text-muted-foreground">Dropped</dt>
                <dd
                  className={`tabular-nums ${health.spans_dropped > 0 ? toneText.danger : ""}`}
                >
                  {health.spans_dropped.toLocaleString()} spans
                </dd>
              </div>
              <div className="flex justify-between sm:flex-col sm:gap-0.5">
                <dt className="text-muted-foreground">Failed</dt>
                <dd
                  className={`tabular-nums ${
                    health.export_failures + health.metric_failures > 0 ? toneText.danger : ""
                  }`}
                >
                  {(health.export_failures + health.metric_failures).toLocaleString()} exports
                </dd>
              </div>
            </dl>

            {health.last_error && (
              <Notice
                tone="danger"
                title={
                  <>
                    Last export error
                    {health.last_error_at && (
                      <>
                        {" "}(<RelativeTime iso={health.last_error_at} />)
                      </>
                    )}
                  </>
                }
              >
                {health.last_error}
              </Notice>
            )}

            {testError && (
              <Notice live tone="danger" title="The collector said">
                {testError}
              </Notice>
            )}
          </div>
        </Section>
      )}

      <Section
        title="Prometheus scrape endpoint"
        description="Set at deployment, not here."
      >
        <div className="space-y-3 p-4">
          <dl className="grid grid-cols-1 gap-3 text-sm sm:grid-cols-2">
            <div className="flex justify-between sm:flex-col sm:gap-0.5">
              <dt className="text-muted-foreground">Address</dt>
              <dd className="font-mono">
                {settings.scrape_enabled ? settings.scrape_addr : "off"}
              </dd>
            </div>
            <div className="flex justify-between sm:flex-col sm:gap-0.5">
              <dt className="text-muted-foreground">Bearer token</dt>
              <dd className="font-mono">
                {settings.scrape_enabled
                  ? settings.scrape_token_set
                    ? "required"
                    : "none"
                  : "—"}
              </dd>
            </div>
          </dl>
        </div>
      </Section>

      <Dialog open={handBack} onOpenChange={setHandBack}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Hand telemetry back to the environment?</DialogTitle>
            <DialogDescription>
              The endpoint saved here is deleted, and exports follow
              OTEL_EXPORTER_OTLP_ENDPOINT — currently {settings.env_endpoint}.
              {settings.headers_set
                ? " The collector credential is deleted too, and can’t be read back."
                : ""}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setHandBack(false)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => {
                setHandBack(false);
                void save(true);
              }}
              disabled={saving}
            >
              Hand it back
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
