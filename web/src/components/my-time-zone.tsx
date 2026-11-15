import { useState } from "react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { FieldRow } from "@/components/field-row";
import { FieldError } from "@/components/field-help";
import { Section } from "@/components/section";
import { useAuth } from "@/hooks/use-auth";
import { api } from "@/lib/api";
import type { TimeZonePreference } from "@/lib/api";
import { knownTimeZones } from "@/lib/schedule";
import { browserTimeZone, formatExactTime, zoneFormattable } from "@/lib/time";
import { fieldErrorId } from "@/lib/utils";

const ZONES = ["UTC", ...knownTimeZones().filter((z) => z !== "UTC")];

/** The picker's four values. "other" is not stored: it opens the zone field. */
type Mode = "" | "browser" | "UTC" | "other";

function modeOf(user: string): Mode {
  if (user === "" || user === "browser" || user === "UTC") return user;
  return "other";
}

/**
 * The zone this person reads every time in (ADR §154). Three layers, shown as
 * what each would resolve to on *this* browser, so nobody has to know what
 * "install default" means to choose it: their own choice, the install's
 * default, this browser's zone. Saves on the pick, like any choice; a named
 * zone saves on its own button, because it is typed.
 */
export function MyTimeZone({ tz }: { tz: TimeZonePreference }) {
  const { refresh } = useAuth();
  const [mode, setMode] = useState<Mode>(modeOf(tz.user));
  const [zone, setZone] = useState(modeOf(tz.user) === "other" ? tz.user : "");
  const [saving, setSaving] = useState(false);

  const browser = browserTimeZone();
  const installLabel = tz.install
    ? `Install default (${tz.install})`
    : `Install default (this browser, ${browser})`;
  const LABEL: Record<Mode, string> = {
    "": installLabel,
    browser: `This browser (${browser})`,
    UTC: "UTC",
    other: "Another zone…",
  };

  const typed = zone.trim();
  const zoneError =
    mode === "other" && typed !== "" && !zoneFormattable(typed)
      ? "Not a time zone name — try one like Europe/London."
      : null;

  async function save(value: string) {
    setSaving(true);
    try {
      await api.setMyTimeZone(value);
      toast.success("Saved");
      // Re-reads /auth/me and re-renders every time on the page in the new zone.
      await refresh();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Couldn’t save the time zone");
    } finally {
      setSaving(false);
    }
  }

  function pick(next: Mode) {
    setMode(next);
    if (next !== "other") void save(next);
  }

  return (
    <Section
      title="Time zone"
      description="Every date and time in Toft is shown in this zone. Exports and the API stay in UTC."
    >
      <div className="space-y-3 p-4">
        <FieldRow
          label="Show times in"
          htmlFor="my-time-zone"
          help={`Now: ${formatExactTime(new Date().toISOString())}`}
        >
          <Select value={mode} onValueChange={(v) => pick((v ?? "") as Mode)} disabled={saving}>
            <SelectTrigger id="my-time-zone" className="w-full">
              <SelectValue>{(v) => LABEL[(v ?? "") as Mode]}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              {(["", "browser", "UTC", "other"] as Mode[]).map((m) => (
                <SelectItem key={m || "install"} value={m}>
                  {LABEL[m]}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </FieldRow>

        {mode === "other" && (
          <FieldRow
            label="Zone"
            htmlFor="my-time-zone-name"
            extra={zoneError && <FieldError field="my-time-zone-name">{zoneError}</FieldError>}
          >
            <div className="flex w-full gap-2">
              <Input
                id="my-time-zone-name"
                value={zone}
                placeholder="Europe/London"
                list="my-time-zone-names"
                disabled={saving}
                aria-invalid={!!zoneError}
                aria-describedby={zoneError ? fieldErrorId("my-time-zone-name") : undefined}
                onChange={(e) => setZone(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" && typed && !zoneError) void save(typed);
                }}
              />
              <datalist id="my-time-zone-names">
                {ZONES.map((z) => (
                  <option key={z} value={z}>
                    {z}
                  </option>
                ))}
              </datalist>
              <Button
                variant="outline"
                disabled={saving || !typed || !!zoneError || typed === tz.user}
                onClick={() => void save(typed)}
              >
                {saving ? "Saving…" : "Save"}
              </Button>
            </div>
          </FieldRow>
        )}
      </div>
    </Section>
  );
}
