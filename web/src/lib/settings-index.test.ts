import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

import { adminGroups } from "./admin-nav";
import { APP_FIELD_CONTROL, APP_FIELD_KEYS, appFieldError, sanitizeSlug } from "./app-settings-fields";
import { SETTINGS_PAGE_KEYS } from "./settings-fields";
import {
  APP_MOVED_ANCHORS,
  APP_SETTING,
  APP_SETTINGS_INDEX,
  APP_SETTINGS_SECTIONS,
  APP_TAB_LABEL,
  SETTINGS_INDEX,
  SETTINGS_SECTIONS,
  appSettingHref,
  appSettingsIn,
  searchAppSettings,
  searchSettings,
  settingHref,
  settingPageLabel,
  settingsIn,
  type AppSettingEntry,
  type AppTab,
  type SettingEntry,
} from "./settings-index";

/* ------------------------------------------------------------------ *
 * The guard
 *
 * A setting in the index and rendered nowhere, or rendered and not in
 * the index, is the failure the index must not permit — because every
 * later move leans on it. Seven settings changing pages is safe only if
 * something notices when one of them arrives nowhere.
 *
 * **One guard, two scopes.** The second half of this file walks the
 * app-scoped index the same way, against the tabs of the app detail
 * page rather than against admin routes — a sibling file would have
 * been two guards that could drift, and the thing being asserted is
 * identical: an entry that points at nothing, or a control nothing
 * claims. It is what makes the app Settings tab's regroup safe, and it
 * is why the index came first.
 *
 * So this walks both directions:
 *
 *   index → page   every entry's anchor exists on the page it names, and
 *                  so does its control unless it is a composite.
 *   page → index   every `id="…"` a registered page writes is an anchor
 *                  the index declares, a control the index claims, or on
 *                  the short allow-list below, each with its reason.
 *
 * It reads the sources rather than rendering them: these pages each need
 * an authenticated administrator and half a dozen endpoints, and the
 * question being asked — does this id exist in this file — does not need
 * a browser to answer. What it therefore cannot see is a control whose
 * `id` is computed, and a *page component* is where that would hide, so
 * the pages here write theirs literally and the test fails loudly if a
 * new one does not.
 * ------------------------------------------------------------------ */

/**
 * Every file that renders a registered page's settings, relative to this file
 * — the page, and the editors it mounts. A setting whose control lives in a
 * component is still that page's setting, and the guard has to be able to see
 * it there.
 */
const PAGE_SOURCE: Record<string, string[]> = {
  "/admin/settings": ["../pages/admin/settings.tsx"],
  "/admin/email": ["../pages/admin/email.tsx"],
  "/admin/telemetry": ["../pages/admin/telemetry.tsx"],
  "/admin/dependencies": ["../pages/admin/dependencies.tsx"],
  "/admin/integrations": ["../pages/admin/integrations.tsx"],
  "/admin/authentication": [
    "../pages/admin/authentication.tsx",
    "../components/anonymous-access-editor.tsx",
    "../components/app-listing-editor.tsx",
    "../components/dormant-lock-editor.tsx",
    "../components/session-policy-editor.tsx",
  ],
  "/admin/api-keys": ["../pages/admin/api-keys.tsx", "../components/personal-keys-switch.tsx"],
  "/admin/features": ["../pages/admin/features.tsx"],
  "/admin/messages": [
    "../pages/admin/messages.tsx",
    "../components/announcement-editor.tsx",
    "../components/access-messages-editor.tsx",
  ],
  "/admin/sources": [
    "../pages/admin/sources.tsx",
    "../components/source-policy-editor.tsx",
    "../components/git-connections.tsx",
  ],
};

/**
 * Literal ids on a registered page that are not settings, each with the
 * reason it is not one. Adding to this list is how a control is declared to
 * be something other than an install setting — it is deliberately short, and
 * deliberately in the test rather than in the page, so that growing it is
 * visible in review.
 */
const NOT_A_SETTING: Record<string, string> = {
  "retention-days-error": "the bounds message under retention-days — the setting's refusal, not a second setting. Written as a literal because the field is not one of the generated rows",
  "provider-id": "a field of one provider's own form, not an install setting",
  "provider-name": "a field of one provider's own form",
  "provider-issuer": "a field of one provider's own form",
  "provider-client-id": "a field of one provider's own form",
  "provider-secret": "a field of one provider's own form",
  "provider-scopes": "a field of one provider's own form",
  "provider-auth-url": "a field of one provider's own form",
  "provider-token-url": "a field of one provider's own form",
  "provider-userinfo-url": "a field of one provider's own form",
  "provider-domains": "a field of one provider's own form",
  "provider-enabled": "a field of one provider's own form",
  "provider-link": "a field of one provider's own form",
  "provider-auto-provision": "a field of one provider's own form",
  "provider-groups-claim": "a field of one provider's own form",
  "provider-emails": "a field of one provider's own form",
  "provider-permitted-groups": "a field of one provider's own form",
  "provider-groups-separator": "a field of one provider's own form",
  "scim-base": "a read-only address in one provider's directory-feed dialog",
  "scim-minted-base": "the same address, beside the token in the once-only dialog",
  "git-webhook-url": "the webhook URL, beside the secret in the once-only dialog",
  "dormant_lock_days-error": "the message under the field, not a second control",
  // Every field of the Connect-a-host dialog. A connection is a row an
  // administrator creates, not a value they set, so the section is the indexed
  // thing (`git_connections`, composite) and these are that form's own fields —
  // the same shape as the provider form above.
  "conn-name": "a field of one connection's own form, not an install setting",
  "conn-kind": "a field of one connection's own form",
  "conn-method": "a field of one connection's own form",
  "conn-base": "a field of one connection's own form",
  "conn-app-id": "a field of one connection's own form",
  "conn-install-id": "a field of one connection's own form",
  "conn-client-id": "a field of one connection's own form",
  "conn-client-secret": "a field of one connection's own form",
  "conn-secret": "a field of one connection's own form",
  "conn-coverage": "a field of one connection's own form",
  "conn-owner": "a field of one connection's own form",
  "conn-expires": "a field of one connection's own form",
  // Every field of the Connect-an-integration dialog, for the reason the
  // provider and git-connection forms above are: a connection is a row an
  // administrator creates, not a value they set, so the section is the indexed
  // thing (`integration_connections`, composite) and these are that form's own
  // fields.
  "new-conn-name": "a field of one connection's own form, not an install setting",
  "new-conn-scope": "a field of one connection's own form",
  "new-conn-description": "a field of one connection's own form",
  "version-op": "the package search's version predicate — a query, not an install setting",
  "version-lower": "the package search's version predicate",
  "version-upper": "the package search's version predicate",
  // Admin → Dependencies' two readouts: what needs a decision, and the state
  // of the advisory data. Lists and a status, not values anybody sets.
  decide: "the Needs a decision list, a readout the policy line links to",
  "advisory-data": "the advisory data's per-ecosystem state, a readout",
  // Ids that exist so a visible `<Label>` can point at the control beside it.
  // A `<label>` with no `htmlFor` is invalid HTML and announces as a label for
  // nothing, so these are the price of the accessibility floor — not settings,
  // and each names a field of a dialog rather than a value of the install.
  "provider-preset": "which preset the Add-provider dialog is seeded from",
  "default-role": "a field of one provider's own provisioning form",
  "redirect-uri": "a read-only value to copy into the provider, not a setting",
  "resource-kind": "a field of the Create-resource dialog",
  "mapping-app": "a field of the mapping dialog — which app, not a setting",
  "mapping-access": "a field of the mapping dialog — at what level",
};

function source(route: string): string {
  return (PAGE_SOURCE[route] ?? [])
    .map((rel) => readFileSync(fileURLToPath(new URL(rel, import.meta.url)), "utf8"))
    .join("\n");
}

/** Every literal `id="…"` a page writes. */
function literalIds(text: string): string[] {
  return [...text.matchAll(/\bid="([a-zA-Z0-9_-]+)"/g)].map((m) => m[1]);
}

const ROUTES = [...new Set(SETTINGS_INDEX.map((e) => e.route))];
const CONTROL = (entry: SettingEntry) => entry.control ?? entry.id;

describe("the settings index is well formed", () => {
  it("names every setting once", () => {
    const seen = new Set<string>();
    for (const entry of SETTINGS_INDEX) {
      expect(seen.has(entry.id), `${entry.id} is indexed twice`).toBe(false);
      seen.add(entry.id);
    }
  });

  it("says what each setting is", () => {
    for (const entry of SETTINGS_INDEX) {
      expect(entry.label.length, `${entry.id} has no label`).toBeGreaterThan(0);
      expect(entry.summary.length, `${entry.id} has no summary`).toBeGreaterThan(0);
      expect(entry.anchor, `${entry.id} has an unusable anchor`).toMatch(/^[a-z][a-z0-9-]*$/);
    }
  });

  it("puts every setting on a page the sidebar can reach", () => {
    const destinations = new Set(adminGroups.flatMap((g) => g.items).map((i) => i.href));
    for (const route of ROUTES) {
      expect(destinations.has(route), `${route} is not an admin destination`).toBe(true);
      expect(settingPageLabel(route), `${route} has no name`).not.toBe(route);
    }
  });

  it("knows the source of every page it claims", () => {
    // Registering a page in the index without registering it here would leave
    // that page unguarded, which is the one hole worth closing loudly.
    for (const route of ROUTES) {
      expect(PAGE_SOURCE[route], `${route} has no source registered for the guard`).toBeTruthy();
    }
  });
});

describe("index → page: every indexed setting has somewhere to land", () => {
  it("has an anchor on the page it names", () => {
    for (const route of ROUTES) {
      const text = source(route);
      const ids = new Set(literalIds(text));
      const anchors = new Set(
        route === "/admin/settings"
          ? SETTINGS_SECTIONS.map((s) => s.id) // rendered from the sections table
          : [],
      );
      for (const entry of SETTINGS_INDEX.filter((e) => e.route === route)) {
        const found = ids.has(entry.anchor) || anchors.has(entry.anchor);
        expect(found, `${entry.id} points at ${settingHref(entry)}, which nothing renders`).toBe(
          true,
        );
      }
    }
  });

  it("has a control on the page it names", () => {
    for (const route of ROUTES) {
      // The Settings page generates its rows from the index rather than
      // writing them out, so its half of this is the exhaustive control map
      // below rather than a literal id.
      if (route === "/admin/settings") continue;
      const ids = new Set(literalIds(source(route)));
      for (const entry of SETTINGS_INDEX.filter((e) => e.route === route && !e.composite)) {
        expect(
          ids.has(CONTROL(entry)),
          `${entry.id} claims a control id="${CONTROL(entry)}" that ${route} does not render`,
        ).toBe(true);
      }
    }
  });

  it("draws a control for every setting the Settings page holds", () => {
    // Both directions at once, and the reason the Settings page can be
    // generated at all: a retries ceiling had copy, a section and a place in
    // the filter's count, and no row was ever drawn for it.
    const indexed = SETTINGS_INDEX.filter((e) => e.route === "/admin/settings" && !e.composite)
      .map((e) => e.id)
      .sort();
    expect([...SETTINGS_PAGE_KEYS].sort()).toEqual(indexed);
  });

  it("puts every Settings-page setting in a section that exists", () => {
    const sections = new Set(SETTINGS_SECTIONS.map((s) => s.id));
    for (const entry of SETTINGS_INDEX.filter((e) => e.route === "/admin/settings")) {
      expect(sections.has(entry.anchor), `${entry.id} is in no section`).toBe(true);
    }
    // …and no section is a heading over nothing. `install` is the exception
    // and says so: it is four read-only rows about how this install was
    // deployed, not settings anybody can change here.
    for (const section of SETTINGS_SECTIONS) {
      if (section.id === "install") continue;
      expect(settingsIn(section.id).length, `${section.id} holds no settings`).toBeGreaterThan(0);
    }
  });
});

describe("page → index: nothing is rendered that the index has not heard of", () => {
  it("indexes every control on every registered page", () => {
    const known = new Set<string>();
    for (const entry of SETTINGS_INDEX) {
      known.add(entry.id);
      known.add(CONTROL(entry));
      known.add(entry.anchor);
    }
    for (const id of SETTINGS_SECTIONS.map((s) => s.id)) known.add(id);

    for (const route of ROUTES) {
      for (const id of literalIds(source(route))) {
        if (known.has(id) || id in NOT_A_SETTING) continue;
        throw new Error(
          `${route} renders id="${id}", which is in no index entry. Add it to ` +
            `SETTINGS_INDEX, or to NOT_A_SETTING with the reason it is not a setting.`,
        );
      }
    }
  });
});

describe("what the three consumers read", () => {
  it("finds a setting by a word that is only in its keywords", () => {
    expect(searchSettings("otlp").map((e) => e.id)).toContain("telemetry_endpoint");
    expect(searchSettings("587").map((e) => e.id)).toContain("email_port");
    expect(searchSettings("break-glass").map((e) => e.id)).toContain("local_admin_enabled");
  });

  it("answers the searches the Settings filter used to deny", () => {
    // §1.3 of the PRD, as a test: these are the words that got "No setting by
    // that name" out of a page that could only ever see its own twenty-one.
    for (const word of ["smtp", "otlp", "budget", "waiver", "issuer"]) {
      expect(searchSettings(word, "/admin/settings").length, `${word} finds nothing`).toBeGreaterThan(
        0,
      );
    }
  });

  it("excludes the asking page from its own cross-page results", () => {
    for (const entry of searchSettings("retention", "/admin/settings")) {
      expect(entry.route).not.toBe("/admin/settings");
    }
  });

  it("says nothing when asked nothing", () => {
    expect(searchSettings("")).toEqual([]);
    expect(searchSettings("   ")).toEqual([]);
  });
});

/* ================================================================== *
 * The same guard, one level down: an app's own settings
 * ================================================================== */

/**
 * Every file that renders an app tab's settings. The Settings tab is the page
 * component plus the tag editor it mounts; Access is the sharing panel.
 *
 * The tabs that render no setting are deliberately absent rather than mapped
 * to an empty list — Integrations, Automation and Dependencies each configure
 * something, but what they configure is an integration, a schedule and a
 * verdict, not a column on `apps`. Registering them would put every field of
 * every dialog on those tabs through the allow-list below for no gain.
 *
 * Overview is absent for the same reason and is now spelled in `AppTab`, which
 * is why this is a Partial: the type gained a tab that holds a *section*
 * (Storage) and no setting. `APP_TABS` is derived from the index's entries, so
 * a tab with none of those is never asked for its source.
 */
const APP_TAB_SOURCE: Partial<Record<AppTab, string[]>> = {
  // The settings tab is its own module since app-detail.tsx was split; the
  // storage section renders from Overview and has one too. This list is the
  // guard's whole view of what the tab renders, so a section that moves to a
  // new file has to be added here or its settings silently stop being checked.
  settings: [
    "../pages/app/settings-tab.tsx",
    "../pages/app/storage-block.tsx",
    "../components/tag-editor.tsx",
    // The notebook re-render schedule, moved off Overview. Its three ids had
    // never been checked by anything, because the file it lived in was not on
    // this list.
    "../components/notebook-render-schedule.tsx",
    // The scheduled restart, [restart] in toft.toml.
    "../components/restart-schedule.tsx",
  ],
  access: [
    "../components/sharing-tab.tsx",
    // The decider's access-request queue, mounted from sharing-tab.tsx and
    // rendering its own section heading and anchor.
    "../components/app-access-requests.tsx",
  ],
};

/**
 * Literal ids on a registered app tab that are not settings on that app, each
 * with the reason. Short and in the test on purpose, exactly as the
 * install-wide list is: growing it is how a control gets declared to be
 * something other than a setting, and that should be visible in review.
 */
const NOT_AN_APP_SETTING: Record<string, string> = {
  "app-preview": "the preview iframe on Overview, not a setting",
  "wrap-toggle": "a view control on the Logs tab — it changes nothing about the app",
  "app-id": "the identifier, read-only and the one thing here nobody can change",
  "repo-url-change":
    "an administrator repointing the app at a different repository — a recovery action, not one of the app's settings",
  "grant-search": "the box that finds people to grant, not a setting",
  "grant-note":
    "the words that ride along with one share, typed and sent and gone — it stores nothing and there is no state of the app it describes",
  "attach-path":
    "where a delegated storage resource lands in this app, typed inside the attach dialog. It belongs to that one mapping — a row in storage_mappings that an administrator can revoke — and not to the app, whose settings survive it",
  "lock-message":
    "what visitors are told, typed inside the lock confirmation — it belongs to that one decision, not to the app. The install-wide default it falls back to is a setting, and is indexed as app_locked_message under Admin \u2192 Messages",
  // The attach dialog's other two fields, named so their visible labels can
  // point at them. Like `attach-path` above, each belongs to one mapping rather
  // than to the app.
  "attach-resource": "which resource the attach dialog is mounting, a field of that dialog",
  "attach-access": "at what level it is mounted, a field of that dialog",
  "attach-folder":
    "which directory of the resource is mounted, a field of that dialog. Part of the mapping's identity rather than of the app: two folders of one resource are two mappings, and revoking one leaves the other",
  "render-schedule-enabled":
    "the On switch beside the cron expression — the same setting suspended, not a second one. It writes render_schedule.enabled, which is why it is indexed under render-schedule and not on its own",
};

function appSource(tab: AppTab): string {
  const files = APP_TAB_SOURCE[tab];
  if (!files) throw new Error(`the ${tab} tab has settings in the index and no source registered`);
  return files
    .map((rel) => readFileSync(fileURLToPath(new URL(rel, import.meta.url)), "utf8"))
    .join("\n");
}

const APP_TABS = [...new Set(APP_SETTINGS_INDEX.map((e) => e.tab))];
const APP_CONTROL = (entry: AppSettingEntry) => entry.control ?? entry.id;

describe("the app settings index is well formed", () => {
  it("names every setting once", () => {
    const seen = new Set<string>();
    for (const entry of APP_SETTINGS_INDEX) {
      expect(seen.has(entry.id), `${entry.id} is indexed twice`).toBe(false);
      seen.add(entry.id);
    }
  });

  it("says what each setting is", () => {
    for (const entry of APP_SETTINGS_INDEX) {
      expect(entry.label.length, `${entry.id} has no label`).toBeGreaterThan(0);
      expect(entry.summary.length, `${entry.id} has no summary`).toBeGreaterThan(0);
      expect(entry.anchor, `${entry.id} has an unusable anchor`).toMatch(/^[a-z][a-z0-9-]*$/);
    }
  });

  it("puts every setting on a tab that has a name and a source", () => {
    for (const tab of APP_TABS) {
      expect(APP_TAB_LABEL[tab], `${tab} has no name`).toBeTruthy();
      expect(APP_TAB_SOURCE[tab], `${tab} has no source registered for the guard`).toBeTruthy();
    }
  });

  it("draws every batched field at the id the index claims for it", () => {
    // Both directions at once, and the reason the pending bar can exist at
    // all: a field renamed in the form contract and not in the index leaves
    // `/apps/{id}/settings#source` pointing at an input that is not there.
    for (const key of APP_FIELD_KEYS) {
      const entry = APP_SETTING[key as keyof typeof APP_SETTING];
      expect(entry, `${key} batches into the pending bar but is in no index entry`).toBeTruthy();
      expect(entry.control, `${key}'s index entry claims no control`).toBe(APP_FIELD_CONTROL[key]);
    }
  });

  it("links to a tab of the app rather than to the app", () => {
    // Overview owns the bare `/apps/{id}`, so a setting whose href lost its
    // segment would land on the hub and silently scroll nowhere.
    for (const entry of APP_SETTINGS_INDEX) {
      expect(appSettingHref("abc", entry)).toBe(`/apps/abc/${entry.tab}#${entry.anchor}`);
    }
  });
});

describe("index → tab: every indexed app setting has somewhere to land", () => {
  it("has an anchor on the tab it names", () => {
    for (const tab of APP_TABS) {
      const ids = new Set(literalIds(appSource(tab)));
      for (const entry of APP_SETTINGS_INDEX.filter((e) => e.tab === tab)) {
        expect(
          ids.has(entry.anchor),
          `${entry.id} points at ${appSettingHref("{id}", entry)}, which nothing renders`,
        ).toBe(true);
      }
    }
  });

  it("has a control on the tab it names", () => {
    // The five batched fields are drawn by one component from an `id` it is
    // handed, so their ids are not literal in the page — exactly as the admin
    // Settings page's generated rows are not. Their half of this is the
    // exhaustive `APP_FIELD_CONTROL` map, held against the index above.
    const batched = new Set<string>(APP_FIELD_KEYS);
    for (const tab of APP_TABS) {
      const ids = new Set(literalIds(appSource(tab)));
      for (const entry of APP_SETTINGS_INDEX.filter(
        (e) => e.tab === tab && !e.composite && !batched.has(e.id),
      )) {
        expect(
          ids.has(APP_CONTROL(entry)),
          `${entry.id} claims a control id="${APP_CONTROL(entry)}" that the ${tab} tab does not render`,
        ).toBe(true);
      }
    }
  });

  it("puts every app setting in a section that exists", () => {
    // Every registered tab, not just Settings. Filtering to one tab is what
    // let the Access tab's Platform lock and Embedding sections stay unknown
    // to the index: two of its five sections were undeclared, so neither was
    // findable by the app's own search and neither could ever have a rail.
    const sections = new Set(APP_SETTINGS_SECTIONS.map((s) => s.id));
    for (const entry of APP_SETTINGS_INDEX) {
      expect(sections.has(entry.anchor), `${entry.id} is in no section`).toBe(true);
    }
  });

  it("holds a setting in every section that claims to have one", () => {
    // `settings: false` is the declaration that a section is read-only facts
    // or actions — Storage, Saved data, the Danger zone. A section without it
    // and with nothing in it is a heading over nothing.
    for (const section of APP_SETTINGS_SECTIONS) {
      if (section.settings === false) continue;
      expect(
        appSettingsIn(section).length,
        `${section.id} holds no settings and does not say it holds none`,
      ).toBeGreaterThan(0);
    }
  });

  it("renders every section the table declares, on the tab it declares", () => {
    for (const tab of APP_TABS) {
      const ids = new Set(literalIds(appSource(tab)));
      for (const section of APP_SETTINGS_SECTIONS.filter((s) => s.tab === tab)) {
        expect(
          ids.has(section.id),
          `section “${section.title}” has no id="${section.id}" on the ${tab} tab`,
        ).toBe(true);
      }
    }
  });

  it("leaves nowhere for a moved anchor to land that does not exist", () => {
    // Every entry in APP_MOVED_ANCHORS names a live destination. A redirect to
    // an anchor nothing renders is worse than no redirect: it moves somebody
    // to a different tab and then abandons them at the top of it.
    const live = new Set(APP_SETTINGS_SECTIONS.map((s) => `${s.tab}#${s.id}`));
    for (const [from, to] of Object.entries(APP_MOVED_ANCHORS)) {
      expect(live.has(`${to.tab}#${to.anchor}`), `#${from} redirects nowhere`).toBe(true);
      expect(live.has(`settings#${from}`), `#${from} is claimed as moved and still exists`).toBe(
        false,
      );
    }
  });
});

describe("tab → index: nothing on an app tab is rendered that the index has not heard of", () => {
  it("indexes every control on every registered tab", () => {
    const known = new Set<string>();
    for (const entry of APP_SETTINGS_INDEX) {
      known.add(entry.id);
      known.add(APP_CONTROL(entry));
      known.add(entry.anchor);
    }
    for (const section of APP_SETTINGS_SECTIONS) known.add(section.id);

    for (const tab of APP_TABS) {
      for (const id of literalIds(appSource(tab))) {
        if (known.has(id) || id in NOT_AN_APP_SETTING) continue;
        throw new Error(
          `the ${tab} tab renders id="${id}", which is in no index entry. Add it to ` +
            `APP_SETTINGS_INDEX, or to NOT_AN_APP_SETTING with the reason it is not a setting.`,
        );
      }
    }
  });
});

describe("what the app-scoped consumers read", () => {
  it("finds a setting by a word that is only in its keywords", () => {
    expect(searchAppSettings("hibernate").map((e) => e.id)).toContain("never_sleep");
    expect(searchAppSettings("dotenv").map((e) => e.id)).toContain("env_vars");
    expect(searchAppSettings("bookmark").map((e) => e.id)).toContain("slug");
  });

  it("answers the searches the Settings tab had no way to answer", () => {
    // §1.6 of the PRD, as a test. There was no filter box on that tab at all,
    // so these were not answered badly — they could not be asked.
    for (const word of ["branch", "sleep", "isolated", "visibility", "auto-deploy"]) {
      expect(searchAppSettings(word).length, `${word} finds nothing`).toBeGreaterThan(0);
    }
  });

  it("sends the tab that cannot answer to the one that can", () => {
    // The concrete case: visibility decides who may open this app's URL, and
    // the Settings tab does not hold it.
    const elsewhere = searchAppSettings("visibility", "settings");
    expect(elsewhere.map((e) => e.id)).toContain("visibility");
    for (const entry of elsewhere) expect(entry.tab).not.toBe("settings");
  });

  it("says nothing when asked nothing", () => {
    expect(searchAppSettings("")).toEqual([]);
    expect(searchAppSettings("   ")).toEqual([]);
  });
});

describe("the app form contract mirrors the server", () => {
  it("sanitizes a slug the way sanitizeSlug does", () => {
    // internal/api/app_handlers.go: lowercase, anything outside [a-z0-9-]
    // becomes a dash, dashes trimmed from both ends, empty becomes "app".
    expect(sanitizeSlug("Ops Console")).toBe("ops-console");
    expect(sanitizeSlug("--Weird__Name!!--")).toBe("weird--name");
    expect(sanitizeSlug("ALLCAPS")).toBe("allcaps");
    expect(sanitizeSlug("!!!")).toBe("app");
    expect(sanitizeSlug("already-fine")).toBe("already-fine");
  });

  it("refuses what the server refuses, and nothing else", () => {
    expect(appFieldError("name", "  ")).toBeTruthy();
    expect(appFieldError("name", "x".repeat(101))).toBeTruthy();
    expect(appFieldError("name", "x".repeat(100))).toBeNull();
    expect(appFieldError("name", "Ops Console")).toBeTruthy();

    // Blank is "follow the platform default", stored as 0 — not an error.
    expect(appFieldError("releases_kept", "")).toBeNull();
    expect(appFieldError("releases_kept", "3")).toBeNull();
    expect(appFieldError("releases_kept", "0")).toBeTruthy();
    expect(appFieldError("releases_kept", "2.5")).toBeTruthy();

    // A write-only credential's blank means "keep what is stored", so an empty
    // git token is never an error — clearing one is its own action.
    expect(appFieldError("git_token", "")).toBeNull();
  });
});
