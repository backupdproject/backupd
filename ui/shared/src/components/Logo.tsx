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

/** The wordmark beside the mark, and it is TYPE rather than art: it inherits
 *  `--font-mono` and the text tokens, so it themes with the surface it sits
 *  on and needs no asset per provider. The drawn lockup in
 *  `docs/assets/logo-*.svg` is the same name as geometry, for the places a
 *  font cannot be relied on; `docs/design/brand-assets.md` records that the
 *  two are deliberately different objects.
 *
 *  The trailing `d` carries the daemon accent, in the muted text tone. That
 *  is the split the previous name had and it survives the rename intact,
 *  because this name also ends in `d` (FR-44, #893). `docs/site/*.html`'s
 *  `.brand-name`/`.brand-dash` header is the same split in the same order,
 *  deliberately. */
export function Wordmark({ size = 14 }: { size?: number }) {
  return (
    <span
      style={{
        fontFamily: "var(--font-mono)", fontWeight: 600,
        fontSize: size, letterSpacing: "-0.01em", whiteSpace: "nowrap"
      }}
    >
      retn<span style={{ color: "var(--text-3)" }}>d</span>
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
