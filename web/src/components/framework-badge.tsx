import { Link } from "react-router-dom";
import { Badge } from "@/components/ui/badge";
import { frameworkMeta } from "@/lib/frameworks";

/**
 * Small neutral badge with a brand-colored dot identifying the framework.
 *
 * This *is* the Framework tag's chip. The framework is a system tag whose value
 * the platform assigns, so rather than render a second "Framework: Next.js"
 * chip beside this badge, the badge itself links to the tag's browse page.
 */
export function FrameworkBadge({
  framework,
  className,
  link = true,
}: {
  framework?: string;
  className?: string;
  link?: boolean;
}) {
  const meta = frameworkMeta(framework);
  if (!meta) return null;

  const classes = `gap-1.5 font-normal text-2xs text-muted-foreground ${className ?? ""}`;
  const body = (
    <>
      <span className="h-1.5 w-1.5 rounded-full" style={{ backgroundColor: meta.color }} aria-hidden />
      {meta.label}
    </>
  );

  if (!link) {
    return <Badge variant="outline" className={classes}>{body}</Badge>;
  }
  return (
    <Badge
      variant="outline"
      className={`${classes} hover:bg-muted`}
      render={<Link to={`/tags/framework/${framework}`} />}
    >
      {body}
    </Badge>
  );
}
