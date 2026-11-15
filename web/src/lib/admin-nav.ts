/**
 * The admin section's destinations, as data.
 *
 * Extracted from `admin-layout.tsx` so the command palette can offer all of
 * them by name. It previously listed three, which meant ⌘K could reach Audit
 * but not Telemetry, Storage or Automation — and the reason was only that
 * somebody had hand-copied a few entries. A second copy of this list would
 * drift the moment a page is added; there is one, and Admin home renders the
 * same one as tiles.
 *
 * **The groups name what you are doing, not what the page is about** (ADR §50
 * D4). They were System / Content / Governance, in tens and fours, and the
 * strain was written down in this file: five comment blocks adjudicating where
 * a single item belonged. Every one of those arguments was correct, and needing
 * five of them for twenty-one items was the finding — the axis did not sort the
 * items, so each addition was decided by hand and the answer was legible only
 * to whoever wrote the comment. "System" mixed configuring with observing;
 * "Content" was Posit Connect's word for apps and here labelled a group holding
 * the build queue and a taxonomy editor.
 *
 * The four below each answer one question, and a new page joins the group whose
 * question it answers:
 *
 *   Overview    what is happening on this machine right now
 *   Apps        what the estate is doing, and what it is given
 *   Platform    what this install offers, and how it speaks
 *   Governance  who and what is allowed, and the record of both
 *
 */

import {
  Activity, CalendarClock, ChartColumn, Code, Cpu, Crosshair, FileClock,
  GitBranch, HardDrive, House, IdCard, Info, Key, KeyRound, ListOrdered, Mail,
  Megaphone, Network, Package, Plug, Rocket, ScrollText, Settings, ShieldCheck,
  Stethoscope, Tag, ToggleRight, UserCheck,
  type LucideIcon,
} from "lucide-react";

export interface AdminNavItem {
  label: string;
  href: string;
  /** Drawn beside the label in the sidebar and the command palette — the
   *  column of icons is what tells a destination from its group's name. */
  icon: LucideIcon;
  /** An optional capability this page needs. The item is absent until the
   *  install answers that it has it — `useAdminGroups`. */
  capability?: "storage";
  /**
   * `form`: the page is settings to read and change, so it is held to the
   * settings measure (`max-w-3xl`) the way Admin → Settings and the app's
   * Settings tab are. Uncapped, Messages' text boxes ran 1,060px and Features'
   * descriptions 1,050px a line, beside Email and Telemetry, which were capped.
   * A page whose payload is a table keeps the full width.
   */
  measure?: "form";
  /**
   * `false`: reachable by name from ⌘K, but not a sidebar entry or an Admin
   * home tile — a section of another page (its href carries the anchor), or a
   * page another page links to. The sidebar had 29 entries and outran most of
   * the pages beside it (ADR §50).
   */
  sidebar?: false;
}

export interface AdminNavGroup {
  label: string;
  items: AdminNavItem[];
}

export const adminGroups: AdminNavGroup[] = [
  {
    label: "Overview",
    items: [
      {
        label: "Home",
        href: "/admin",
        icon: House,
      },
      {
        label: "System info",
        href: "/admin/info",
        icon: Info,
      },
      {
        label: "System checks",
        href: "/admin/checks",
        icon: Stethoscope,
      },
      {
        label: "Processes",
        href: "/admin/processes",
        icon: Cpu,
      },
    ],
  },
  {
    label: "Estate",
    items: [
      {
        label: "Usage",
        href: "/admin/usage",
        icon: ChartColumn,
      },
      {
        label: "Languages",
        href: "/admin/info#languages",
        icon: Code,
        sidebar: false,
      },
      {
        label: "Scan targets",
        href: "/admin/estate/targets",
        icon: Crosshair,
      },
      {
        label: "Build queue",
        href: "/admin/processes#queue",
        icon: ListOrdered,
        sidebar: false,
      },
      {
        label: "Automation",
        href: "/admin/automation",
        icon: CalendarClock,
      },
      {
        label: "Tags",
        href: "/admin/tags",
        icon: Tag,
      },
      {
        label: "Storage",
        href: "/admin/storage",
        icon: HardDrive,
        capability: "storage",
      },
    ],
  },
  {
    label: "Platform",
    items: [
      {
        label: "Settings",
        href: "/admin/settings",
        icon: Settings,
      },
      {
        label: "Features",
        href: "/admin/features",
        measure: "form",
        icon: ToggleRight,
      },
      {
        label: "Email",
        href: "/admin/email",
        measure: "form",
        icon: Mail,
      },
      {
        label: "Telemetry",
        href: "/admin/telemetry",
        measure: "form",
        icon: Activity,
      },
      {
        label: "Integrations",
        href: "/admin/integrations",
        measure: "form",
        icon: Plug,
      },
      {
        label: "Messages",
        href: "/admin/messages",
        measure: "form",
        icon: Megaphone,
      },
    ],
  },
  {
    label: "Governance",
    items: [
      {
        label: "Authentication",
        href: "/admin/authentication",
        measure: "form",
        icon: KeyRound,
      },
      {
        label: "Sources",
        href: "/admin/sources",
        measure: "form",
        icon: GitBranch,
      },
      {
        label: "Dependencies",
        href: "/admin/dependencies",
        icon: Package,
      },
      {
        label: "Publisher requests",
        href: "/admin/publisher-requests",
        icon: UserCheck,
      },
      {
        label: "Reach",
        href: "/admin/reach",
        icon: Network,
      },
      {
        label: "Compliance",
        href: "/admin/compliance",
        icon: ShieldCheck,
      },
      {
        label: "Credentials",
        href: "/admin/credentials",
        icon: IdCard,
      },
      {
        label: "API keys",
        href: "/admin/api-keys",
        icon: Key,
      },
      {
        label: "Deploy keys",
        href: "/admin/deploy-keys",
        icon: Rocket,
      },
      {
        label: "Activity",
        href: "/admin/audit",
        icon: ScrollText,
      },
      {
        label: "Integration log",
        href: "/admin/integration-log",
        icon: FileClock,
        sidebar: false,
      },
    ],
  },
];
