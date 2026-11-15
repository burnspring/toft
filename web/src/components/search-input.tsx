import { useState, useEffect, useRef, type KeyboardEvent } from "react";
import { Search, X } from "lucide-react";
import { Input } from "@/components/ui/input";

interface SearchInputProps {
  placeholder?: string;
  value?: string;
  onChange: (value: string) => void;
  debounceMs?: number;
  className?: string;
  /** The field's accessible name. Defaults to the placeholder without its
   *  ellipsis, so "Search apps…" is announced as "Search apps". */
  "aria-label"?: string;
  /** Keys the caller handles — a find box's Enter and Shift+Enter. Escape is
   *  this component's and is not passed on while there is text to clear. */
  onKeyDown?: (e: KeyboardEvent<HTMLInputElement>) => void;
}

/**
 * A debounced search box.
 *
 * Two things here exist to stop it from talking back to its parent when nobody
 * typed anything. The handler is held in a ref rather than depended on, because
 * callers pass inline arrows — a new identity every render — and depending on it
 * re-armed the debounce on *every* parent render, firing `onChange` a quarter of
 * a second later. Where the handler also reset something (the People page reset
 * pagination on search), that made the reset arrive unprompted and the page
 * silently snap back. And the last emitted value is remembered so mounting, or
 * syncing a controlled value, does not echo a change that did not happen.
 *
 * **Clearing is not typing, so it does not wait.** The × and Escape both empty
 * the field and tell the parent in the same tick, because the debounce exists to
 * avoid a request per keystroke and getting the whole list back is one action,
 * already decided. Waiting a quarter of a second to undo a filter reads as a
 * control that did not work. Focus stays in the field afterwards: the reason to
 * clear a filter is almost always to type a different one.
 */
export function SearchInput({
  placeholder = "Search…",
  value: controlledValue,
  onChange,
  debounceMs = 250,
  className = "",
  "aria-label": ariaLabel,
  onKeyDown,
}: SearchInputProps) {
  const [internal, setInternal] = useState(controlledValue ?? "");
  // The last value the parent was told. Anything else is a change nobody made.
  const [emitted, setEmitted] = useState(internal);

  const onChangeRef = useRef(onChange);
  useEffect(() => {
    onChangeRef.current = onChange;
  });

  // Adjusting state during render is React's own answer to "a prop changed and
  // some state derives from it"; an effect for this costs an extra render.
  const [lastControlled, setLastControlled] = useState(controlledValue);
  if (controlledValue !== undefined && controlledValue !== lastControlled) {
    setLastControlled(controlledValue);
    setInternal(controlledValue);
    // The parent already knows: emitting it back would be an echo.
    setEmitted(controlledValue);
  }

  useEffect(() => {
    if (internal === emitted) return;
    const timer = setTimeout(() => {
      setEmitted(internal);
      onChangeRef.current(internal);
    }, debounceMs);
    return () => clearTimeout(timer);
  }, [internal, emitted, debounceMs]);

  const inputRef = useRef<HTMLInputElement>(null);

  // Emitting here as well as setting `emitted` keeps the debounce effect a no-op
  // for this change — it sees internal === emitted and never fires a second time.
  const clear = () => {
    setInternal("");
    setEmitted("");
    onChangeRef.current("");
    inputRef.current?.focus();
  };

  return (
    <div className={`relative ${className}`}>
      <Search className="absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground/60" />
      <Input
        ref={inputRef}
        placeholder={placeholder}
        aria-label={ariaLabel ?? placeholder.replace(/(…|\.\.\.)$/, "")}
        value={internal}
        onChange={(e) => setInternal(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Escape" && internal !== "") {
            // Only when there is something to clear: an empty field must let
            // Escape through to whatever surface is listening for it.
            e.stopPropagation();
            clear();
            return;
          }
          onKeyDown?.(e);
        }}
        // The right padding is unconditional. Several callers let the field size
        // itself, so padding that appeared with the × widened the box as you
        // typed and shrank it as you cleared, shoving the control beside it
        // sideways. Reserved space costs nothing; a jumping filter bar does.
        className="pl-9 pr-8 bg-card"
      />
      {internal && (
        <button
          type="button"
          onClick={clear}
          aria-label="Clear search"
          className="absolute right-1.5 top-1/2 flex h-5 w-5 -translate-y-1/2 items-center justify-center rounded text-muted-foreground transition-colors hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
        >
          <X className="h-3.5 w-3.5" />
        </button>
      )}
    </div>
  );
}
