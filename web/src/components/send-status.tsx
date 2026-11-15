import { Badge } from "@/components/ui/badge";
import { toneSurface, type Tone } from "@/lib/tone";

/**
 * How a send ended.
 *
 * Five values rather than two, because the ledger row is written before the
 * send is attempted and the ways it can fail are not the same problem:
 * `blocked` is an administrator withdrawing the capability mid-flight,
 * `abandoned` is a platform restart losing a queued message, `failed` is a
 * transport that said no. An owner reading this needs to know which.
 *
 * Quiet in flight, `ok` once delivered, loud for anything that did not arrive —
 * except `blocked`, which is `attention`: nothing malfunctioned, an
 * administrator withdrew the capability and the row is the record of that. Red
 * would send an owner to debug a message the platform refused on purpose.
 */
const SEND_STATUS: Record<string, { label: string; tone: Tone }> = {
  queued: { label: "Queued", tone: "neutral" },
  sent: { label: "Sent", tone: "ok" },
  failed: { label: "Failed", tone: "danger" },
  blocked: { label: "Blocked", tone: "attention" },
  abandoned: { label: "Abandoned", tone: "danger" },
};

export function SendStatusBadge({ status }: { status: string }) {
  const meaning = SEND_STATUS[status] ?? { label: status, tone: "neutral" as Tone };
  return (
    <Badge variant="outline" className={toneSurface[meaning.tone]}>
      {meaning.label}
    </Badge>
  );
}
