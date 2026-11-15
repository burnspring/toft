import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { FieldError, FieldHelp } from "@/components/field-help";
import { fieldErrorId } from "@/lib/utils";
import { knownTimeZones, zoneLooksValid } from "@/lib/schedule";

const ZONES = knownTimeZones();

/**
 * The zone a schedule is read in. A text field with the browser's zone list as
 * suggestions rather than a select of four hundred names: people type "Lon" and
 * pick. Empty is UTC.
 */
export function TimeZoneField({
  id,
  value,
  onChange,
  disabled,
}: {
  id: string;
  value: string;
  onChange: (zone: string) => void;
  disabled?: boolean;
}) {
  const valid = zoneLooksValid(value, ZONES);
  return (
    <div className="space-y-2">
      <Label htmlFor={id}>Time zone</Label>
      <Input
        id={id} value={value} placeholder="UTC" list={`${id}-zones`} disabled={disabled}
        aria-invalid={!valid} aria-describedby={valid ? undefined : fieldErrorId(id)}
        onChange={(e) => onChange(e.target.value)}
      />
      <datalist id={`${id}-zones`}>
        {ZONES.map((z) => (
          <option key={z} value={z}>
            {z}
          </option>
        ))}
      </datalist>
      {valid ? (
        <FieldHelp>The schedule runs at this wall-clock time, through daylight saving changes.</FieldHelp>
      ) : (
        <FieldError field={id}>Not a time zone name — try one like Europe/London.</FieldError>
      )}
    </div>
  );
}
