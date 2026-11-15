import { usePresentSections } from "@/hooks/use-section-spy";
/**
 * "On this page", with the section you are looking at marked.
 *
 * The recovered width, and what it is for. A long form's fields do not get
 * better at 1,232 pixels — label-and-field pairs scan worse that wide, not
 * better — so a page that has grown past one screen spends the space beside
 * them on a way of not losing your place in it. Admin → Settings has had one
 * since ADR §50; the app's Settings tab occupied 47% of the width for 2.9
 * screens of scrolling and spent the rest on nothing.
 *
 * Hidden below `lg`, where there is no width to recover and the sections are
 * a short scroll apart anyway.
 *
 * **It lists what is on the page, not what the table says could be.** The
 * sections come from a static index, and several of them render conditionally
 * — the app's Access tab has a Platform lock only for an administrator or a
 * locked app, and a Waiting-for-you only while somebody is waiting — so the
 * rail offered destinations that were not there, and `jump` below returned
 * early on each. A link that silently does nothing reads as a broken page.
 * `usePresentSections` asks the DOM, rather than each rail re-deriving
 * conditions that live at the call site.
 */
export function SectionRail({
  sections,
  active,
  label = "Sections",
}: {
  sections: { id: string; title: string }[];
  /** The id of the last heading scrolled past. */
  active: string;
  label?: string;
}) {
  const present = usePresentSections(sections.map((s) => s.id));
  const shown = sections.filter((s) => present.has(s.id));

  function jump(e: React.MouseEvent<HTMLAnchorElement>, id: string) {
    const el = document.getElementById(id);
    if (!el) return;
    e.preventDefault();
    const smooth = !window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    el.scrollIntoView({ behavior: smooth ? "smooth" : "auto", block: "start" });
    history.replaceState(null, "", `#${id}`);
    // **And take focus with it.** Preventing the default kept the page from
    // jumping, and it also kept the browser from doing the other half of what
    // a fragment link does: somebody who activated a rail item with the
    // keyboard was left in the rail, and the next Tab carried on down the rail
    // rather than into the section they had just asked for. `preventScroll`,
    // because the smooth scroll above is already on its way there.
    el.setAttribute("tabindex", "-1");
    el.focus({ preventScroll: true });
  }

  // Nothing on the page yet, or nothing that renders: no rail rather than an
  // empty one under a heading promising sections.
  if (shown.length === 0) return null;

  return (
    <nav aria-label={label} className="hidden w-44 shrink-0 lg:block">
      <div className="sticky top-20">
        <p className="mb-2 px-3 text-xs font-medium text-muted-foreground">
          On this page
        </p>
        <ul className="border-l border-border/60">
          {shown.map((s) => (
            <li key={s.id}>
              <a
                href={`#${s.id}`}
                onClick={(e) => jump(e, s.id)}
                aria-current={active === s.id ? "location" : undefined}
                className={`-ml-px block border-l-2 px-3 py-1.5 text-sm transition-colors ${
                  active === s.id
                    ? "border-primary font-medium text-foreground"
                    : "border-transparent text-muted-foreground hover:border-border hover:text-foreground"
                }`}
              >
                {s.title}
              </a>
            </li>
          ))}
        </ul>
      </div>
    </nav>
  );
}
