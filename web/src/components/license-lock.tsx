import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";

import { api, TIER_LABEL, type Feature } from "@/lib/api";
import { Notice } from "@/components/notice";
import { buttonVariants } from "@/components/ui/button-variants";

/**
 * The banner a configuration surface wears when this install is not licensed
 * for it.
 *
 * # Locked is not absent, and this is the only place that distinction lives
 *
 * Everywhere else in this product, a capability the install does not offer is
 * **absent**: the routes 404 or explain themselves and nothing appears in the
 * UI (`docs/invariants.md` § Optional capabilities). A paywalled surface is the
 * exact opposite — visible, labelled, one line of what it does and one way to
 * unlock it — because a feature nobody can see sells nothing.
 *
 * The two states are opposite to a reader of the screen and identical to a
 * reader of the code, which is why the distinction is written down rather than
 * left to a convention: **not configured is absent; not licensed is locked.** A
 * surface that is both renders locked, because telling somebody to configure
 * something they cannot use is the worse of the two wrong answers.
 *
 * # What it deliberately does not do
 *
 * **It does not hide the page.** The read verbs are ungated on the server, so
 * whatever is already configured here is still on screen, still readable, and
 * still working — what a license decides is whether *new* configuration may be
 * written. A locked page that went blank would say a license took something
 * down, and nothing here ever does.
 *
 * **It does not disable the controls.** A disabled control is not focusable and
 * swallows mouse events, so the reason for it is unreachable by every route;
 * the server refuses the write in its own words and the toast carries them.
 * This notice is what makes that refusal predictable rather than surprising.
 *
 * `info` rather than `attention`: nothing is wrong, nothing has stopped, and
 * nobody has done anything they should not have. Reaching for a warning colour
 * to mean "you could buy this" is how a warning colour stops working.
 */
export function LicenseLock({ feature }: { feature: string }) {
  // The features list is admin-only and every page that mounts this is too,
  // except Groups — where a publisher sees no notice rather than a failed read,
  // which is right: the license is not their business and the server still
  // refuses the write in words they can act on.
  const { data } = useQuery({
    queryKey: ["features"],
    queryFn: api.listFeatures,
    retry: false,
  });

  const f = (data ?? []).find((x: Feature) => x.name === feature);
  if (!f || f.licensed) return null;

  return (
    // **Changing this**, not "this". The title has to be exact about what the
    // gate covers or the page contradicts itself two lines later, and it
    // sidesteps a subject that may be plural — "Identity providers needs" reads
    // as a bug in the product.
    <Notice
      tone="info"
      size="md"
      className="mb-6"
      title={`Changing this needs ${TIER_LABEL[f.min_tier]}`}
      action={
        <Link
          to="/admin/features"
          className={buttonVariants({ variant: "outline", size: "sm" })}
        >
          See features
        </Link>
      }
    >
      This install is on {TIER_LABEL[f.tier]}. What’s already set up keeps working.
    </Notice>
  );
}
