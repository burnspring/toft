import { Hash, Lock } from "lucide-react";

import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import type { IntegrationDestination } from "@/lib/api";

/**
 * The destination menu, shared by the Integrations page and an app's own tab.
 *
 * The empty value is a real choice on the app side ("use the connection's
 * default"), so it is offered rather than being the absence of a selection.
 *
 * It is one component for every kind that enumerates, because what a picker
 * needs is a list of ids and labels and nothing about which provider produced
 * them. `hash` renders the `#` a chat channel is written with; a document
 * folder gets a folder icon and no prefix.
 */
export function DestinationSelect({
  id,
  value,
  onChange,
  destinations,
  disabled,
  placeholder,
  defaultLabel,
  hash = false,
}: {
  id?: string;
  value: string;
  onChange: (v: string) => void;
  destinations: IntegrationDestination[];
  disabled?: boolean;
  placeholder: string;
  defaultLabel?: string;
  hash?: boolean;
}) {
  const prefix = hash ? "#" : "";
  const label = (v: string) => {
    if (!v) return defaultLabel ?? placeholder;
    const d = destinations.find((x) => x.id === v);
    return d ? `${prefix}${d.label}` : v;
  };
  return (
    <Select value={value} onValueChange={(v) => onChange(v ?? "")} disabled={disabled}>
      <SelectTrigger id={id} className="w-full max-w-sm">
        <SelectValue>{(v) => label(v as string)}</SelectValue>
      </SelectTrigger>
      <SelectContent>
        {defaultLabel !== undefined && <SelectItem value="">{defaultLabel}</SelectItem>}
        {destinations.map((d) => (
          <SelectItem key={d.id} value={d.id}>
            <span className="inline-flex items-center gap-1.5">
              {hash ? (
                d.id.startsWith("G") || d.label.startsWith("private") ? (
                  <Lock className="h-3 w-3 text-muted-foreground" aria-hidden />
                ) : (
                  <Hash className="h-3 w-3 text-muted-foreground" aria-hidden />
                )
              ) : null}
              {d.label || d.id}
            </span>
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}
