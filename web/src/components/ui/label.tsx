"use client"

import * as React from "react"

import { cn } from "@/lib/utils"

const labelClass =
  "flex items-center gap-2 text-sm leading-none font-medium select-none group-data-[disabled=true]:pointer-events-none group-data-[disabled=true]:opacity-50 peer-disabled:cursor-not-allowed peer-disabled:opacity-50"

function Label({ className, ...props }: React.ComponentProps<"label">) {
  return <label data-slot="label" className={cn(labelClass, className)} {...props} />
}

/**
 * A field's name where there is no single control to name.
 *
 * A `<label>` must point at something; a heading over a *group* of buttons, or
 * over a composite like the ZIP dropzone, points at nothing — which is invalid
 * HTML and announces to a screen reader as a label with no field. Those places
 * were using `Label` because it was the thing that looked right, so this is the
 * thing that looks right and is not a label. The group itself carries the
 * accessible name, on its `role="group" aria-label`.
 */
function FieldLabel({ className, ...props }: React.ComponentProps<"span">) {
  return <span data-slot="field-label" className={cn(labelClass, className)} {...props} />
}

export { Label, FieldLabel }
