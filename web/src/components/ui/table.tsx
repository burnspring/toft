"use client"

import * as React from "react"

import { cn } from "@/lib/utils"

/**
 * The scroll container is a **container-query context**, and that is the whole
 * reason this wrapper is not just a `div` with `overflow-x-auto`.
 *
 * A table's responsive decisions are about *this* box, never about the window.
 * A column written `hidden lg:table-cell` was asking whether the viewport had
 * reached 1,024 — but the space it was going to occupy is the card's, and the
 * card is a 1,232px page, or an admin column with a 232px rail beside it, or
 * an app tab with its own rail, or a dialog. Those disagree by hundreds of
 * pixels at any one viewport width, so a single class meant four different
 * amounts of room and was right in at most one of them. The admin console was
 * the worst of the four and the one people noticed: its content column used to
 * stop growing at 998px, so `xl:table-cell` added a column at the exact width
 * the container reached its ceiling, and the table scrolled sideways on a
 * screen with nine hundred spare pixels beside it.
 *
 * With `@container` here, the `@`-prefixed variants every column in the product
 * now uses measure the container, and one set of names is correct in all four
 * places. The tiers, in the order a table sheds them:
 *
 * | Variant | Container | Roughly |
 * |---|---|---|
 * | (none) | any | the identity column, and the one carrying the answer |
 * | `@md` | 448px | a dialog, a phone, a narrow panel |
 * | `@xl` | 576px | an admin page beside its rail on a laptop |
 * | `@3xl` | 768px | a full-width page on a laptop |
 * | `@4xl` | 896px | an admin page on a desktop, and everything wider |
 *
 * `@4xl` is the last tier an admin page reaches, because 1,232px is where its
 * content column lands and stays (`layout.tsx`). A column needing more room
 * than that is not a narrow-screen problem to defer — it is a column this
 * product has nowhere to put, and it belongs under the row's first cell.
 */
function Table({ className, ...props }: React.ComponentProps<"table">) {
  return (
    <div
      data-slot="table-container"
      className="@container relative w-full overflow-x-auto"
    >
      <table
        data-slot="table"
        className={cn("w-full caption-bottom text-sm", className)}
        {...props}
      />
    </div>
  )
}

function TableHeader({ className, ...props }: React.ComponentProps<"thead">) {
  return (
    <thead
      data-slot="table-header"
      className={cn("[&_tr]:border-b", className)}
      {...props}
    />
  )
}

function TableBody({ className, ...props }: React.ComponentProps<"tbody">) {
  return (
    <tbody
      data-slot="table-body"
      className={cn("[&_tr:last-child]:border-0", className)}
      {...props}
    />
  )
}

function TableFooter({ className, ...props }: React.ComponentProps<"tfoot">) {
  return (
    <tfoot
      data-slot="table-footer"
      className={cn(
        "border-t bg-muted/50 font-medium [&>tr]:last:border-b-0",
        className
      )}
      {...props}
    />
  )
}

function TableRow({ className, ...props }: React.ComponentProps<"tr">) {
  return (
    <tr
      data-slot="table-row"
      className={cn(
        "border-b transition-colors hover:bg-muted/50 data-[state=selected]:bg-muted",
        className
      )}
      {...props}
    />
  )
}

function TableHead({ className, ...props }: React.ComponentProps<"th">) {
  return (
    <th
      data-slot="table-head"
      className={cn(
        "h-9 px-2 text-left align-middle text-xs font-medium whitespace-nowrap text-muted-foreground [&:has([role=checkbox])]:pr-0",
        className
      )}
      {...props}
    />
  )
}

function TableCell({ className, ...props }: React.ComponentProps<"td">) {
  return (
    <td
      data-slot="table-cell"
      className={cn(
        "h-10 px-2 py-1.5 align-middle whitespace-nowrap [&:has([role=checkbox])]:pr-0",
        className
      )}
      {...props}
    />
  )
}

function TableCaption({
  className,
  ...props
}: React.ComponentProps<"caption">) {
  return (
    <caption
      data-slot="table-caption"
      className={cn("mt-4 text-sm text-muted-foreground", className)}
      {...props}
    />
  )
}

export {
  Table,
  TableHeader,
  TableBody,
  TableFooter,
  TableHead,
  TableRow,
  TableCell,
  TableCaption,
}
