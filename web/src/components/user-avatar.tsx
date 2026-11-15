import { Avatar, AvatarFallback, AvatarImage } from "@/components/ui/avatar";
import { accentHue } from "@/lib/frameworks";
import { initials } from "@/lib/initials";
import { cn } from "@/lib/utils";

const sizes = {
  xs: ["size-5", "text-2xs"],
  sm: ["size-7", "text-2xs"],
  md: ["size-8", "text-xs"],
  lg: ["size-12", "text-sm"],
} as const;

/**
 * A person, as a picture when the provider sent one and otherwise as two
 * letters on a hue derived from their id — the same recipe `AppIcon` uses, so
 * two people whose initials collide still read as two people.
 */
export function UserAvatar({
  id,
  login,
  name,
  avatarUrl,
  size = "sm",
  className,
}: {
  id?: string;
  login?: string;
  name?: string;
  avatarUrl?: string;
  size?: keyof typeof sizes;
  className?: string;
}) {
  const hue = accentHue(id || login || name || "");
  return (
    <Avatar className={cn("shrink-0", sizes[size][0], className)}>
      {avatarUrl ? <AvatarImage src={avatarUrl} /> : null}
      <AvatarFallback
        className={cn("font-medium", sizes[size][1])}
        style={{ backgroundColor: `oklch(0.94 0.045 ${hue})`, color: `oklch(0.45 0.16 ${hue})` }}
      >
        {initials(name, login)}
      </AvatarFallback>
    </Avatar>
  );
}
