// What the landing page may say an app can send to, and the only place it is
// said. `landing-integrations.test.ts` walks this against `appint.Known` in
// internal/appint/catalog.go, in both directions — the same arrangement
// `landing-frameworks.ts` has with the detector, and for the same reason.
//
// This is a claim about a code path: a kind absent from the catalog has no
// transport, no refusal copy and no settings-index entry, so naming one here
// would advertise a destination an app cannot reach. The invariant is already
// written down — *the list is the specification* — and this is that list read
// from the other side of the language boundary.
//
// The titles are the catalog's own, character for character, so the product
// spells "Microsoft Teams" once. They are not sorted here: the catalog's order
// is a decision (Slack and email lead because they are the two that predate it)
// and re-ordering them on this page would be a second opinion about it.

export const LANDING_INTEGRATIONS: readonly string[] = [
  "Slack",
  "Email",
  "Signed webhook",
  "SharePoint",
  "OneDrive",
  "Microsoft Teams",
  "Microsoft Teams (bot)",
  "Google Drive",
  "Google Chat",
  "Discord",
  "PagerDuty",
];

/**
 * The page groups the names by what they are for. A name with no group here
 * fails `landing-integrations.test.ts`, so a kind added to the catalog cannot
 * reach the list and silently miss the page.
 */
export const LANDING_INTEGRATION_GROUPS: readonly {
  label: string;
  names: readonly string[];
}[] = [
  {
    label: "Messages",
    names: ["Slack", "Microsoft Teams", "Microsoft Teams (bot)", "Google Chat", "Discord", "Email"],
  },
  { label: "Files", names: ["SharePoint", "OneDrive", "Google Drive"] },
  { label: "Alerts and webhooks", names: ["PagerDuty", "Signed webhook"] },
];
