import { AccessTable } from "@/components/access-table";
import { Section } from "@/components/section";
import type { UserAccessReport } from "@/lib/api";

/**
 * What you can reach, and why.
 *
 * The same report an administrator reads about somebody on `/people/:id`,
 * rendered by the same component from the same server-side builder. If apps
 * owned by a group you belong to ever start appearing here, that is group
 * ownership arriving through the existing `SourceChip` machinery with no change
 * to this file — which is the test of whether reading the report was the right
 * move instead of re-deriving reach from grants.
 */
export function MeAccessTab({
  access,
  error,
  onRetry,
}: {
  access: UserAccessReport | undefined;
  error?: unknown;
  onRetry?: () => void;
}) {
  return (
    <Section title="Apps">
      <AccessTable report={access} voice="self" error={error} onRetry={onRetry} />
      {/* The honest limit of the page. Everything above is a report; none of it
          is a control, and saying where the controls are is more useful than
          leaving somebody to look for them here. */}
      <p className="border-t border-border/60 px-4 py-3 text-xs text-muted-foreground">
        Not listed? Find the app under Apps and request access, or ask its owner.
      </p>
    </Section>
  );
}
