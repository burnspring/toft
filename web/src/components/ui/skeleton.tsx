import { cn } from "@/lib/utils";

function Skeleton({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="skeleton"
      // Before the spread, so a caller can still override it. A pulsing grey
      // rectangle has nothing in it to read out, and a page full of them read
      // as a wall of unlabelled regions.
      aria-hidden="true"
      className={cn("animate-pulse rounded-md bg-muted", className)}
      {...props}
    />
  );
}

export { Skeleton };
