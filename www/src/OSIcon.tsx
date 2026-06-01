// OS marks for the Machines table. `uname -s` gives "Darwin"/"Linux". macOS
// gets the Apple mark (simple-icons); Linux/unknown get a clean terminal glyph
// — no penguin.
const APPLE_D =
  'M12.152 6.896c-.948 0-2.415-1.078-3.96-1.04-2.04.027-3.91 1.183-4.961 3.014-2.117 3.675-.546 9.103 1.519 12.09 1.013 1.454 2.208 3.09 3.792 3.039 1.52-.065 2.09-.987 3.935-.987 1.831 0 2.35.987 3.96.948 1.637-.026 2.676-1.48 3.676-2.948 1.156-1.688 1.636-3.325 1.662-3.415-.039-.013-3.182-1.221-3.22-4.857-.026-3.04 2.48-4.494 2.597-4.559-1.429-2.09-3.623-2.324-4.39-2.376-2-.156-3.675 1.09-4.61 1.09zM15.53 3.83c.843-1.012 1.4-2.427 1.245-3.83-1.207.052-2.662.805-3.532 1.818-.78.896-1.454 2.338-1.273 3.714 1.338.104 2.715-.688 3.559-1.701'

export function OSIcon({ os, size = 16, className = '' }: { os?: string; size?: number; className?: string }) {
  const norm = (os ?? '').toLowerCase()
  if (norm === 'darwin') {
    return (
      <svg width={size} height={size} viewBox="0 0 24 24" fill="currentColor" className={className} role="img" aria-label="macOS"><path d={APPLE_D} /></svg>
    )
  }
  // Linux / unknown → a terminal window (clean, monochrome, line style)
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round" className={className} role="img" aria-label={norm === 'linux' ? 'Linux' : 'machine'}>
      <rect x="2.5" y="4" width="19" height="16" rx="2" />
      <path d="M6.5 9.5l3 2.5-3 2.5" />
      <line x1="12.5" y1="15" x2="17" y2="15" />
    </svg>
  )
}
