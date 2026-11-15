/**
 * A number field's unit, in a fixed 5rem column — the width of the memory
 * control's MB/GB select — drawn whether or not there is a unit. With each
 * unit at its own width ("m", "seconds", "deployments") every field in a card
 * started at a different x and the column of numbers zigzagged down the page.
 */
export function UnitLabel({ unit }: { unit?: string }) {
  return <span className="w-20 shrink-0 text-left text-xs text-muted-foreground">{unit}</span>;
}
