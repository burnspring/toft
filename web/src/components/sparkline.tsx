interface SparklineProps {
  data: number[];
  width?: number;
  height?: number;
  /** A CSS colour. Defaults to the primary token — never a hex literal, which
   * is what this held before and what made it the one line in the product that
   * would not follow a palette change. */
  color?: string;
  /** What this line is of, as a sentence. The whole of a table column was one
   *  unlabelled `<svg>`: an image with no accessible name is announced as
   *  nothing, so the shape carrying a column's entire payload was absent to a
   *  screen reader rather than merely hard to read. */
  label: string;
}

export function Sparkline({
  data, label, width = 80, height = 24, color = "var(--primary)",
}: SparklineProps) {
  if (data.length < 2) return null;

  const max = Math.max(...data);
  const min = Math.min(...data);
  const range = max - min || 1;
  const padding = 2;

  const points = data
    .map((val, i) => {
      const x = padding + (i / (data.length - 1)) * (width - padding * 2);
      const y = padding + (1 - (val - min) / range) * (height - padding * 2);
      return `${x},${y}`;
    })
    .join(" ");

  return (
    <svg
      width={width}
      height={height}
      className="inline-block"
      role="img"
      aria-label={label}
    >
      <polyline
        fill="none"
        stroke={color}
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeLinejoin="round"
        points={points}
      />
    </svg>
  );
}
