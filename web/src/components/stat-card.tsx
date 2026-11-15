import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { toneSolid, toneText, type Tone } from "@/lib/tone";
import { cn } from "@/lib/utils";
import { Card, CardContent } from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";

interface StatCardProps {
  label: string;
  value: string | number;
  icon?: ReactNode;
  description?: ReactNode;
  trend?: { value: string; positive: boolean };
  /**
   * The figure's own state, when it has one.
   *
   * A tile reporting a peak against a ceiling, or a count of things that broke,
   * is saying something — and it used to say it in the same ink as a version
   * number. Leave it off wherever the number is just a number: a tile that is
   * always coloured is a tile whose colour means nothing.
   */
  tone?: Tone;
  /**
   * A reading against a ceiling, 0–100, drawn as a bar under the figure in the
   * figure's tone. This is the one tile: Processes' host resources had a
   * fourth figure-tile shape of their own (14px value, icon left, a bar), one
   * sidebar click from this one.
   */
  meter?: number;
  /**
   * Where the figure goes when it is something a reader acts on. A count a
   * reader cannot open is a fact stated to somebody who cannot act on it.
   */
  to?: string;
}

export function StatCard({ label, value, icon, description, trend, tone, meter, to }: StatCardProps) {
  const figure = (
    <span className={cn("text-2xl font-semibold tracking-tight tabular-nums", tone && toneText[tone])}>
      {value}
    </span>
  );
  return (
    // `py-0` because `Card` brings its own vertical padding and the content
    // brought more: a tile holding one number stood 135px tall, and System
    // info's eight took two screens on a phone.
    <Card className="py-0">
      <CardContent className="p-4">
        <div className="flex items-center justify-between">
          <span className="text-sm font-medium text-muted-foreground">{label}</span>
          {icon && <span className="text-muted-foreground/60">{icon}</span>}
        </div>
        <div className="mt-2 flex items-baseline gap-2">
          {to ? (
            <Link to={to} className="hover:underline" aria-label={`${label}: ${value}`}>
              {figure}
            </Link>
          ) : (
            figure
          )}
          {trend && (
            <span
              className={`text-xs font-medium ${trend.positive ? toneText.ok : toneText.danger}`}
            >
              {trend.value}
            </span>
          )}
        </div>
        {meter !== undefined && (
          <Progress
            value={meter}
            className="mt-3 h-2"
            indicatorClassName={tone ? toneSolid[tone] : undefined}
          />
        )}
        {description && (
          <p className={cn("text-xs text-muted-foreground", meter !== undefined ? "mt-2" : "mt-1")}>
            {description}
          </p>
        )}
      </CardContent>
    </Card>
  );
}
