import svgBody from './orchid-spray.svg'

/// Botanical illustration: a Phalaenopsis cascade traced from a
/// Rijksmuseum-style vintage line drawing (via potrace). Adopts theme
/// color via currentColor on the inline SVG fills. Bleeds off the left
/// edge so only the right portion of the bloom cascade is visible.
///
/// The raw SVG body lives in `orchid-spray.svg.ts` (default export); the
/// canonical source SVG file is `.claude/skills/orchid-art/orchid-spray.svg`.
export function OrchidArt({
  className = '',
  posClassName = 'fixed left-0 top-1/2 z-0',
  height = '110vh',
  width = '1100px',
  opacity = 0.32,
  bleed = 38,
  maskStart = 55,
  maskEnd = 95,
  transform,
}: {
  className?: string
  posClassName?: string
  height?: string
  width?: string
  opacity?: number
  bleed?: number
  maskStart?: number
  maskEnd?: number
  transform?: string
}) {
  const mask = `linear-gradient(to right, black 0%, black ${maskStart}%, transparent ${maskEnd}%)`
  return (
    <div
      aria-hidden
      className={'pointer-events-none text-blue-700 dark:text-blue-400 ' + posClassName + ' ' + className}
      style={{
        width,
        height,
        opacity,
        maskImage: mask,
        WebkitMaskImage: mask,
        transform: transform ?? `translate(-${bleed}%, -50%)`,
      }}
    >
      <svg
        viewBox="0 0 679.947817 579.194039"
        xmlns="http://www.w3.org/2000/svg"
        width="100%"
        height="100%"
        preserveAspectRatio="xMidYMid meet"
        dangerouslySetInnerHTML={{ __html: svgBody }}
      />
    </div>
  )
}
