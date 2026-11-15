import { accentHue } from "@/lib/frameworks";

const sizes = {
  sm: "h-7 w-7 rounded-md text-2xs",
  md: "h-9 w-9 rounded-lg text-xs",
  lg: "h-11 w-11 rounded-lg text-sm",
} as const;

/**
 * Deterministic identity tile for an app: initials on a pastel background
 * whose hue is derived from the slug, so every app is visually distinct.
 */
export function AppIcon({ name, seed, size = "md" }: { name: string; seed?: string; size?: keyof typeof sizes }) {
  const hue = accentHue(seed ?? name);
  const initials = name
    .split(/[\s_-]+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((w) => w[0]!.toUpperCase())
    .join("") || "?";
  return (
    <div
      aria-hidden
      className={`flex shrink-0 select-none items-center justify-center font-semibold ${sizes[size]}`}
      style={{
        backgroundColor: `oklch(0.94 0.045 ${hue})`,
        color: `oklch(0.45 0.16 ${hue})`,
      }}
    >
      {initials}
    </div>
  );
}
