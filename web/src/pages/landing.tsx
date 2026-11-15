import { useQuery } from "@tanstack/react-query";
import {
  ArrowRight,
  KeyRound,
  LayoutGrid,
  Rocket,
  type LucideIcon,
} from "lucide-react";
import { Link } from "react-router-dom";

import { Code } from "@/components/code";
import { PublicFrame } from "@/components/layout/public-frame";
import { TableCard } from "@/components/table-card";
import { buttonVariants } from "@/components/ui/button-variants";
import { useDocumentTitle } from "@/hooks/use-document-title";
import { publicConfig } from "@/lib/app-url";

/**
 * What an install shows somebody who is not signed in: the front door.
 *
 * The third route outside the `Layout`, after `/login` and `/cli`, and it
 * inherits their reasoning rather than restating it: an unauthenticated visitor
 * reaching a blank screen has no way to tell a broken deployment from a locked
 * door, so this has an error boundary of its own.
 *
 * **Its reader is already inside the organization** (ADR §65). Somebody was
 * sent a link, or typed the address a colleague gave them, and wants to know
 * what happens after they sign in. The case for the product — the languages,
 * the integrations, where it runs, the security review — is gettoft.com's job,
 * because the person deciding whether to adopt Toft never arrives here. So
 * the page is one screen, and small: the outcome beside a sign-in card —
 * `/login`'s shape, so the first two screens anybody sees are one product —
 * and one quiet row of what signing in gives you. Its reader's organization
 * has already chosen Toft, so nothing on it sells: no deploy diagram, no link
 * to the product's site, no band that argues for the product.
 *
 * Three rules govern what may be written here:
 *
 * - **It describes the product, never this install.** No app count, no install
 *   name, no version, no user. On a public address an app count is an inventory
 *   hint, and this page is served to whoever can reach the host.
 * - **It asserts no policy it does not read.** Who may deploy and whether an
 *   app can be requested are this install's decisions, so each cell says what
 *   happens *if* they were made that way rather than that they were.
 * - **It renders no input and asks nothing of its own.** Its one read is the
 *   public config the SPA already fetched at boot, for the documentation link,
 *   which is absent until that answer lands and absent under DOCS_URL=off.
 *
 * Visually it may go wider and larger than the app does, and takes no colour,
 * font or component the app does not already have. The `toft-ui` skill's
 * outside-spa.md § The landing page holds the boundary.
 */
export function LandingPage() {
  useDocumentTitle();

  return (
    <PublicFrame action={<SignIn size="sm" />}>
      <div className="flex flex-1 flex-col justify-center">
        <Hero />
        <AfterSignIn />
      </div>
    </PublicFrame>
  );
}

/** The page's one width, and the gutter that keeps a phone off the edges. */
const CONTAINER = "mx-auto w-full max-w-6xl px-4 sm:px-6";

/**
 * The only place the page raises its voice: display type larger than any
 * heading in the app, the second line drawn in the brand's tone, with the way
 * in beside it in one `TableCard` — the card `/login` draws, in the same place
 * — lit from behind by a soft glow of the same `--primary`. The glow is the
 * page's one decoration: a token, not a colour of its own, and `aria-hidden`.
 *
 * The one entrance on the page is here, through the `tw-animate-css` utilities
 * the dialogs already use, and only for a reader who has not asked for reduced
 * motion. **A rise, never a fade**: a fade starts from nothing, and a link
 * preview or a headless capture taken before it ends is a page with no hero.
 */
function Hero() {
  return (
    <section className={`${CONTAINER} pt-12 pb-10 sm:pt-16 sm:pb-12`}>
      <div className="grid gap-10 motion-safe:animate-in motion-safe:slide-in-from-bottom-3 motion-safe:duration-700 lg:grid-cols-12 lg:items-center lg:gap-16">
        <div className="lg:col-span-7">
          <h1 className="text-4xl leading-[1.05] font-semibold tracking-tighter text-balance sm:text-5xl lg:text-6xl">
            Your team’s apps.{" "}
            <span className="bg-linear-to-r from-primary to-foreground bg-clip-text text-transparent sm:block">
              Behind one sign-in.
            </span>
          </h1>
          <p className="mt-6 max-w-xl text-lg leading-relaxed text-pretty text-muted-foreground">
            Dashboards, reports, tools and APIs built by people in your
            organization.
          </p>
        </div>
        <div className="relative lg:col-span-5">
          <div
            aria-hidden="true"
            className="pointer-events-none absolute -inset-10 rounded-full bg-radial from-primary/30 via-primary/10 to-transparent blur-2xl"
          />
          <TableCard className="relative w-full p-6 shadow-lg shadow-primary/10 ring-1 ring-primary/10 sm:p-8">
            <h2 className="text-lg font-semibold tracking-tight">
              Sign in with your organization’s account
            </h2>
            <p className="mt-2 text-sm leading-relaxed text-pretty text-muted-foreground">
              You’ll see the apps that have been shared with you.
            </p>
            <div className="mt-6 flex flex-wrap gap-3">
              <SignIn size="lg" />
              <ReadTheGuide />
            </div>
            <p className="mt-6 border-t border-border/60 pt-4 text-sm text-pretty text-muted-foreground">
              Can’t sign in, or can’t find an app you expected? Ask your
              administrator.
            </p>
          </TableCard>
        </div>
      </div>
    </section>
  );
}

interface Cell {
  title: string;
  body: React.ReactNode;
  icon: LucideIcon;
}

/**
 * What signing in gives you, one short line each. Each is a conditional where
 * the install decides, so none of them is false on an install that decided the
 * other way.
 */
const CELLS: Cell[] = [
  {
    title: "Open your apps",
    body: "Every app shared with you, on the one sign-in.",
    icon: LayoutGrid,
  },
  {
    title: "Ask for access",
    body: "Request it from the app’s page. Its owners decide.",
    icon: KeyRound,
  },
  {
    title: "Deploy your own",
    body: (
      <>
        If your administrator allows it, from Git or the <Code>toft</Code> CLI.
      </>
    ),
    icon: Rocket,
  },
];

/** A quiet row under the hero, not a band: no card, hairlines between. */
function AfterSignIn() {
  return (
    <section className={`${CONTAINER} pb-12 sm:pb-16`}>
      <h2 className="sr-only">After you sign in</h2>
      <ul className="grid gap-6 border-t border-border/60 pt-6 md:grid-cols-3 md:gap-8">
        {CELLS.map((cell) => (
          <li key={cell.title} className="flex gap-3">
            <cell.icon
              className="mt-0.5 h-4 w-4 shrink-0 text-primary"
              aria-hidden="true"
            />
            <div>
              <h3 className="text-sm font-semibold tracking-tight">
                {cell.title}
              </h3>
              <p className="mt-1 text-sm leading-relaxed text-pretty text-muted-foreground">
                {cell.body}
              </p>
            </div>
          </li>
        ))}
      </ul>
    </section>
  );
}

/**
 * The call to action, in the header and the hero. There is no self-service
 * account creation here, so there is no "Get started" beside it; the one other
 * way on is the guide.
 */
function SignIn({ size }: { size: "sm" | "lg" }) {
  return (
    <Link
      to="/login"
      // The product's button, given the room a hero asks for. Padding and
      // height only: a marketing surface may be more generous with space than
      // the app is, and may not invent a second button to be generous with.
      className={buttonVariants({
        size,
        className: size === "lg" ? "h-11 px-6 text-base" : undefined,
      })}
    >
      Sign in
      {size === "lg" ? (
        <ArrowRight data-icon="inline-end" aria-hidden="true" />
      ) : null}
    </Link>
  );
}

/** The manuals' folder for this minor, or nothing until and unless it is named. */
function useDocsUrl(): string | undefined {
  const { data } = useQuery({
    queryKey: ["public-config"],
    queryFn: publicConfig,
    staleTime: Infinity,
  });
  return data?.docs_url || undefined;
}

/**
 * The public manuals for this install's minor, as an outlined button beside
 * Sign in. Rendered only once the config has answered with a site, so it never
 * appears and then vanishes, and never appears under DOCS_URL=off, where an
 * air-gapped visitor could not follow it.
 */
function ReadTheGuide() {
  const docsUrl = useDocsUrl();
  if (!docsUrl) return null;
  return (
    <a
      href={docsUrl}
      target="_blank"
      rel="noopener noreferrer"
      className={buttonVariants({
        variant: "outline",
        size: "lg",
        className: "h-11 px-6 text-base",
      })}
    >
      Read the guide
    </a>
  );
}
