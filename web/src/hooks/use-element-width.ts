import { useEffect, useRef, useState } from "react";

/**
 * The rendered width of an element, kept current by a ResizeObserver. A chart
 * draws in this many units so one unit is one CSS pixel: a fixed viewBox
 * stretched with `w-full` scales its type with the card, and 10-unit labels
 * drew at 19px on a wide screen. `fallback` is the width until the first
 * measurement, so the first frame is drawn at a plausible size.
 */
export function useElementWidth<T extends Element>(fallback = 640) {
  const ref = useRef<T>(null);
  const [width, setWidth] = useState(fallback);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const measure = () => {
      const w = Math.round(el.getBoundingClientRect().width);
      if (w > 0) setWidth(w);
    };
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  return [ref, width] as const;
}
