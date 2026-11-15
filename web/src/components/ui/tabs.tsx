import * as React from "react"
import { Tabs as TabsPrimitive } from "@base-ui/react/tabs"
import { cva, type VariantProps } from "class-variance-authority"
import { ChevronLeft, ChevronRight } from "lucide-react"

import { cn } from "@/lib/utils"

function Tabs({
  className,
  orientation = "horizontal",
  ...props
}: TabsPrimitive.Root.Props) {
  return (
    <TabsPrimitive.Root
      data-slot="tabs"
      data-orientation={orientation}
      className={cn(
        "group/tabs flex gap-2 data-horizontal:flex-col",
        className
      )}
      {...props}
    />
  )
}

/**
 * `max-w-full` and the horizontal scroll are not decoration.
 *
 * The list is `inline-flex w-fit` with `whitespace-nowrap` triggers and had no
 * scroll container, so a strip of nine tabs simply overran the card it sits in
 * on a narrow window — the tabs past the edge were unreachable rather than
 * merely cramped, which is a navigation surface disappearing on a phone. It
 * scrolls now, and `[scrollbar-width:none]` keeps the bar out of the layout on
 * a desktop where it never scrolls; the content is reachable either way,
 * because it scrolls by touch, by wheel and by focusing a trigger with the
 * keyboard.
 *
 * **Start-aligned when horizontal.** `justify-center` on an overflowing flex
 * row spills the content past the *start* edge as well, where neither
 * scrolling nor `scrollIntoView` can reach it: at 390px the app page's
 * Overview tab sat at x=-200 in a scroller whose `scrollLeft` was 0.
 *
 * Vertical lists keep wrapping as they did — the overflow only applies to the
 * horizontal orientation, where it is the axis that runs out.
 */
const tabsListVariants = cva(
  "group/tabs-list inline-flex w-fit items-center justify-center rounded-lg p-[3px] text-muted-foreground group-data-horizontal/tabs:h-8 group-data-vertical/tabs:h-fit group-data-vertical/tabs:flex-col data-[variant=line]:rounded-none group-data-horizontal/tabs:max-w-full group-data-horizontal/tabs:overflow-x-auto group-data-horizontal/tabs:justify-start group-data-horizontal/tabs:[scrollbar-width:none] group-data-horizontal/tabs:[&::-webkit-scrollbar]:hidden",
  {
    variants: {
      variant: {
        default: "bg-muted",
        line: "gap-1 bg-transparent",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  }
)

/**
 * Two things the scroll container above does not do on its own.
 *
 * **The active tab is brought into view.** A strip of nine that scrolls is only
 * navigation while you can see where you are; on a phone the app detail page
 * opened with the window resting on `Deployments`, four tabs away from the
 * Overview actually being shown, and nothing on screen said which one was
 * active. Landing on a deep link — or being redirected to Overview because a
 * tab was not yours — put the answer off the edge.
 *
 * **And the overflow is made visible.** The scrollbar is hidden by design, so
 * the only remaining cue that there is more strip is the fade, which appears on
 * the side that has something behind it and on no side when the whole strip
 * fits.
 */
function useHorizontalTabAffordances(
  ref: React.RefObject<HTMLDivElement | null>,
  enabled: boolean,
) {
  const [edges, setEdges] = React.useState({ start: false, end: false })

  React.useEffect(() => {
    const el = ref.current
    if (!el || !enabled) return

    const measure = () => {
      const slack = el.scrollWidth - el.clientWidth
      // A pixel of tolerance: sub-pixel layout leaves a permanent 0.5px of
      // "scrollable" on strips that plainly are not.
      setEdges({
        start: el.scrollLeft > 1,
        end: slack > 1 && el.scrollLeft < slack - 1,
      })
    }

    const reveal = () => {
      const active = el.querySelector<HTMLElement>("[data-active]")
      active?.scrollIntoView({ block: "nearest", inline: "nearest" })
      measure()
    }

    reveal()
    el.addEventListener("scroll", measure, { passive: true })

    const resize = new ResizeObserver(measure)
    resize.observe(el)

    // base-ui moves `data-active` between triggers rather than remounting them,
    // so a route change is an attribute change and nothing else.
    const active = new MutationObserver(reveal)
    active.observe(el, {
      subtree: true,
      attributes: true,
      attributeFilter: ["data-active"],
    })

    return () => {
      el.removeEventListener("scroll", measure)
      resize.disconnect()
      active.disconnect()
    }
  }, [ref, enabled])

  return edges
}

function TabsList({
  className,
  variant = "default",
  ...props
}: TabsPrimitive.List.Props & VariantProps<typeof tabsListVariants>) {
  const ref = React.useRef<HTMLDivElement | null>(null)
  const edges = useHorizontalTabAffordances(ref, true)

  // A mask rather than an overlay: the strip is the scroll container itself, so
  // there is nowhere to hang a gradient without restructuring every tab surface
  // in the product. Vertical lists never overflow on this axis, so both edges
  // read false and the mask is `none`.
  const fade =
    edges.start || edges.end
      ? `linear-gradient(to right, transparent 0, #000 ${edges.start ? "1.5rem" : "0"}, #000 calc(100% - ${edges.end ? "1.5rem" : "0px"}), transparent 100%)`
      : undefined

  // A fade says there is more; it does not say how to get it on a desktop
  // with no touch and no horizontal wheel. So the faded edge also carries a
  // chevron that pages the strip. Pointer-only and out of the tab order: the
  // keyboard already walks the triggers with the arrow keys, and a reader
  // moving by Tab would meet two buttons that duplicate what Arrow does.
  const page = (dir: 1 | -1) => {
    const el = ref.current
    if (el) el.scrollBy({ left: dir * el.clientWidth * 0.7, behavior: "smooth" })
  }
  const chevron =
    "absolute top-1/2 z-10 flex size-6 -translate-y-1/2 items-center justify-center rounded-full border border-border/60 bg-card text-muted-foreground shadow-sm hover:text-foreground"

  return (
    <div className="relative min-w-0 max-w-full">
      <TabsPrimitive.List
        ref={ref}
        data-slot="tabs-list"
        data-variant={variant}
        className={cn(tabsListVariants({ variant }), className)}
        style={fade ? { maskImage: fade, WebkitMaskImage: fade } : undefined}
        {...props}
      />
      {edges.start && (
        <button type="button" tabIndex={-1} aria-hidden className={cn(chevron, "left-0")} onClick={() => page(-1)}>
          <ChevronLeft className="size-3.5" />
        </button>
      )}
      {edges.end && (
        <button type="button" tabIndex={-1} aria-hidden className={cn(chevron, "right-0")} onClick={() => page(1)}>
          <ChevronRight className="size-3.5" />
        </button>
      )}
    </div>
  )
}

function TabsTrigger({ className, ...props }: TabsPrimitive.Tab.Props) {
  return (
    <TabsPrimitive.Tab
      data-slot="tabs-trigger"
      className={cn(
        "relative inline-flex h-[calc(100%-1px)] flex-1 items-center justify-center gap-1.5 rounded-md border border-transparent px-1.5 py-0.5 text-sm font-medium whitespace-nowrap text-foreground/60 transition-all group-data-vertical/tabs:w-full group-data-vertical/tabs:justify-start hover:text-foreground focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-1 focus-visible:outline-ring disabled:pointer-events-none disabled:opacity-50 aria-disabled:pointer-events-none aria-disabled:opacity-50 dark:text-muted-foreground dark:hover:text-foreground group-data-[variant=default]/tabs-list:data-active:shadow-sm group-data-[variant=line]/tabs-list:data-active:shadow-none [&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
        "group-data-[variant=line]/tabs-list:bg-transparent group-data-[variant=line]/tabs-list:data-active:bg-transparent dark:group-data-[variant=line]/tabs-list:data-active:border-transparent dark:group-data-[variant=line]/tabs-list:data-active:bg-transparent",
        "data-active:bg-background data-active:text-foreground dark:data-active:border-input dark:data-active:bg-input/30 dark:data-active:text-foreground",
        "after:absolute after:bg-foreground after:opacity-0 after:transition-opacity group-data-horizontal/tabs:after:inset-x-0 group-data-horizontal/tabs:after:bottom-[-5px] group-data-horizontal/tabs:after:h-0.5 group-data-vertical/tabs:after:inset-y-0 group-data-vertical/tabs:after:-right-1 group-data-vertical/tabs:after:w-0.5 group-data-[variant=line]/tabs-list:data-active:after:opacity-100",
        className
      )}
      {...props}
    />
  )
}

function TabsContent({ className, ...props }: TabsPrimitive.Panel.Props) {
  return (
    <TabsPrimitive.Panel
      data-slot="tabs-content"
      className={cn("flex-1 text-sm outline-none", className)}
      {...props}
    />
  )
}

// tabsListVariants stays internal, for the reason badge.tsx gives.
export { Tabs, TabsList, TabsTrigger, TabsContent }
