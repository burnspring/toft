import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { FieldError, FieldHelp } from "@/components/field-help";
import { fieldErrorId } from "@/lib/utils";

/**
 * The folder of a mapping, on both attach dialogs — the administrator's and a
 * delegate's — so the two cannot ask for it differently.
 *
 * **On a partitioned resource the folder is the mapping**: required, and never
 * inside or around a folder another app is mapped to, because each folder is
 * its own gate. The server holds both rules and names what is in the way; its
 * refusal is shown here, under the field it is about, rather than in a toast
 * that leaves the dialog asking the same question.
 */
export function StorageFolderField({
  id, partitioned, value, onChange, refusal,
}: {
  id: string;
  partitioned: boolean;
  value: string;
  onChange: (v: string) => void;
  /** The server's sentence, when it refused this folder or this app's count. */
  refusal?: string;
}) {
  return (
    <div className="space-y-1.5">
      <Label htmlFor={id}>{partitioned ? "Folder" : "Folder (optional)"}</Label>
      <Input
        id={id}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={partitioned ? "teams/sales" : "reports/2026"}
        className="font-mono text-sm"
        aria-invalid={refusal ? true : undefined}
        aria-describedby={refusal ? fieldErrorId(id) : undefined}
      />
      <FieldError field={id}>{refusal}</FieldError>
      {!refusal && (
        <FieldHelp>
          {partitioned
            ? "Apps on the same folder share it. A folder can’t sit inside another attached one."
            : "Attach one folder instead of everything; the app can’t see outside it. Leave blank to attach the whole resource."}
        </FieldHelp>
      )}
    </div>
  );
}
