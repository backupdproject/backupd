/** Option 1a "Cycle" — the selected mark. A broken ring reads as a transfer
 *  cycle in progress and survives 16px. Colour comes from currentColor so the
 *  provider accent token drives it with no per-provider asset.
 *
 *  `title` is the accessible name and nothing else. It used to be rendered
 *  as an SVG <title> child as well, which names the mark to a screen
 *  reader but ALSO has the browser draw it as a hover tooltip — and this
 *  mark sits in the app header and on the sign-in screen, which #829 says
 *  carries no tooltips at all. `aria-label` on role="img" is the same name
 *  to a screen reader with nothing drawn on hover, so the name survives
 *  the tooltip preference rather than depending on it. */
export function Logo({ size = 24, title }: { size?: number; title?: string }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 48 48"
      role={title ? "img" : undefined}
      aria-hidden={title ? undefined : true}
      aria-label={title}
      style={{ color: "var(--accent)", flex: "none" }}
    >
      <circle
        cx="24" cy="24" r="17" fill="none" stroke="currentColor"
        strokeWidth={size <= 20 ? 5.5 : 5} strokeLinecap="round"
        strokeDasharray="47 20" transform="rotate(-52 24 24)"
      />
      <circle cx="24" cy="24" r={size <= 20 ? 6 : 5.5} fill="currentColor" />
    </svg>
  );
}

export function Wordmark({ size = 14 }: { size?: number }) {
  return (
    <span
      style={{
        fontFamily: "var(--font-mono)", fontWeight: 600,
        fontSize: size, letterSpacing: "-0.01em", whiteSpace: "nowrap"
      }}
    >
      backup<span style={{ color: "var(--text-3)" }}>d</span>
    </span>
  );
}

export function Lockup({ size = 24 }: { size?: number }) {
  return (
    <span style={{ display: "inline-flex", alignItems: "center", gap: 10 }}>
      <Logo size={size} />
      <Wordmark size={size * 0.58 + 0} />
    </span>
  );
}
