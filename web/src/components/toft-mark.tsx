/**
 * Toft's mark — a roof over a block, drawn in `currentColor` so the caller's
 * text colour sets it. `web/public/favicon.svg`, `apple-touch-icon.png` and the
 * gateway cards' header (`internal/api/proxy_handler.go`) carry the same paths;
 * a change here is a change to all four.
 */
export function ToftMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 32 32" className={className} aria-hidden="true">
      <path
        d="M4.5 14.5 16 5l11.5 9.5"
        fill="none"
        stroke="currentColor"
        strokeWidth="3.6"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      <rect x="8" y="17.5" width="16" height="10" rx="2.2" fill="currentColor" />
    </svg>
  );
}
