import { PageHeader } from "@/components/page-header";
import { LicenseLock } from "@/components/license-lock";
import { SourcePolicyEditor } from "@/components/source-policy-editor";
import { GitConnectionsSection } from "@/components/git-connections";
import { useHashScroll } from "@/hooks/use-hash-scroll";

/**
 * Admin → Sources: which repositories this install will deploy from at all.
 *
 * A page rather than a section, because it already was one: a mode, a pattern
 * list, an uploads switch, its own Save, its own Discard, and an impact preview
 * that names the apps which would stop deploying — rendered inside another
 * page's scroll, under a bar that could not save it (ADR §50 D2).
 *
 * Connected git hosts sit below it. The policy says which repositories this
 * install will deploy from; a connection says how it reaches them, and between
 * them they are one question — which is why they are one page and not two.
 *
 * The copy has one job it must not fail, and moving it does not change it. This
 * constrains where an app's code comes from — its provenance — and nothing
 * about what that code then does. A build still runs arbitrary code out of an
 * allowed repository and still installs from public registries.
 */
export function AdminSourcesPage() {
  useHashScroll();

  return (
    <div>
      <PageHeader
        title="Sources"
        description="Which repositories apps may deploy from."
      />

      <LicenseLock feature="trusted_sources" />

      {/* No wrapper `id` here: `SourcePolicyEditor` carries `id="sources"`
          itself, and two elements with one id meant `#sources` from ⌘K and from
          the settings index resolved to whichever the browser found first. */}
      <SourcePolicyEditor />

      {/* Below the policy, because the policy is the constraint and this is how
          the install reaches what the policy allows. */}
      <div className="mt-8">
        <GitConnectionsSection />
      </div>
    </div>
  );
}
