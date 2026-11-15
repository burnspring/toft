import { Building2, Globe, LockKeyhole } from "lucide-react";

/**
 * Who can open an app — the **effective** answer, never the stored column.
 *
 * A public app on an install that has switched anonymous access off is
 * `authenticated` here, with `withheld` set
 * to say why. Reporting the stored word would send a scanner to test signed-out
 * access the platform refuses, and the refusal would come back as a finding
 * about an app behaving exactly as configured.
 *
 * One component because Admin → Reach and Admin → Scan targets each held a copy,
 * and the words had already drifted into jargon in both.
 */
export function EffectiveAudience({
  visibility,
  withheld,
}: {
  visibility: string;
  withheld?: boolean;
}) {
  if (visibility === "public") {
    return (
      <span className="inline-flex items-center gap-1.5 text-sm">
        <Globe className="h-3.5 w-3.5 text-muted-foreground" aria-hidden />
        Public
      </span>
    );
  }
  if (visibility === "authenticated") {
    return (
      <span className="inline-flex items-center gap-1.5 text-sm">
        <Building2 className="h-3.5 w-3.5 text-muted-foreground" aria-hidden />
        Anyone signed in
        {withheld && (
          <span className="text-2xs text-muted-foreground">
            — set to Public, which this install has switched off
          </span>
        )}
      </span>
    );
  }
  return (
    <span className="inline-flex items-center gap-1.5 text-sm">
      <LockKeyhole className="h-3.5 w-3.5 text-muted-foreground" aria-hidden />
      Restricted
    </span>
  );
}
