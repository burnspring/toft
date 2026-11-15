import { Badge } from "@/components/ui/badge";
import { statusMeaning } from "@/lib/status";
import { toneSurface, type Tone } from "@/lib/tone";

interface StatusBadgeProps {
  status: string;
  /** Override the word. The colour still comes from the status. */
  label?: string;
  /**
   * Override the colour, for a domain whose statuses are its own —
   * `jobRunStatusTone`. Such a vocabulary genuinely
   * differs ("ran out of time" is not "failed"), so it maps to a tone
   * itself rather than borrowing a status word that means something else
   * on another page.
   */
  tone?: Tone;
}

/**
 * A state, as a chip. The words and the colours are in `lib/status.ts`; this
 * file is only the shape.
 */
export function StatusBadge({ status, label, tone }: StatusBadgeProps) {
  const meaning = statusMeaning(status);
  return (
    <Badge variant="outline" className={`font-medium text-xs transition-colors duration-300 ${toneSurface[tone ?? meaning.tone]}`}>
      {label ?? meaning.label}
    </Badge>
  );
}
