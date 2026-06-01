// OS marks for the Machines table (from simple-icons, single currentColor
// path). `uname -s` gives "Darwin"/"Linux"; anything else falls back to a
// neutral server glyph so a machine never renders blank.
const APPLE_D =
  'M12.152 6.896c-.948 0-2.415-1.078-3.96-1.04-2.04.027-3.91 1.183-4.961 3.014-2.117 3.675-.546 9.103 1.519 12.09 1.013 1.454 2.208 3.09 3.792 3.039 1.52-.065 2.09-.987 3.935-.987 1.831 0 2.35.987 3.96.948 1.637-.026 2.676-1.48 3.676-2.948 1.156-1.688 1.636-3.325 1.662-3.415-.039-.013-3.182-1.221-3.22-4.857-.026-3.04 2.48-4.494 2.597-4.559-1.429-2.09-3.623-2.324-4.39-2.376-2-.156-3.675 1.09-4.61 1.09zM15.53 3.83c.843-1.012 1.4-2.427 1.245-3.83-1.207.052-2.662.805-3.532 1.818-.78.896-1.454 2.338-1.273 3.714 1.338.104 2.715-.688 3.559-1.701'

const LINUX_D =
  'M14.62 8.35c-.42.28-1.75.84-1.75.84-.43.27-.18.92-.18.92.42-.51 1.65-.85 1.65-.85.86-.34.7-1.18.7-1.18-.18.46-.42.45-.42.45zm-3.6 0s-.24.01-.42-.45c0 0-.16.84.7 1.18 0 0 1.23.34 1.65.85 0 0 .25-.65-.18-.92 0 0-1.33-.56-1.75-.84-.01 0-.01 0 0 0zM12 0C7.94 0 6.36 3.7 6.46 6.16c.07 1.65-.12 2.45-.49 3.34-.42 1-.85 1.96-1.39 2.79-.55.84-.95 1.66-1.13 2.3-.18.6-.13 1.06-.04 1.32.04.13.11.21.18.29-.06.19-.04.43.06.6.16.31.5.43.85.55.71.18 1.66.12 2.41.55.81.41 1.62.59 2.27.41.45-.1.81-.4 1.01-.81.36 0 1.07-.23 1.97-.29.61-.05 1.37.23 2.24.17.02.12.06.18.12.31.39.78 1.12 1.13 1.89 1.07.77-.06 1.59-.53 2.26-1.3.55-.66 1.46-.94 2.07-1.31.3-.18.55-.41.57-.74.02-.35-.17-.71-.62-1.2v-.08c-.15-.18-.22-.47-.3-.81-.07-.35-.16-.69-.43-.92-.05-.04-.1-.06-.16-.11-.04-.03-.1-.04-.16-.06.37-1.11.23-2.22-.15-3.21-.46-1.23-1.27-2.29-1.89-3.03-.69-.87-1.37-1.7-1.36-2.92.02-1.87.2-5.33-3.07-5.34z'

export function OSIcon({ os, size = 16, className = '' }: { os?: string; size?: number; className?: string }) {
  const norm = (os ?? '').toLowerCase()
  if (norm === 'darwin') {
    return (
      <svg width={size} height={size} viewBox="0 0 24 24" fill="currentColor" className={className} role="img" aria-label="macOS"><path d={APPLE_D} /></svg>
    )
  }
  if (norm === 'linux') {
    return (
      <svg width={size} height={size} viewBox="0 0 24 24" fill="currentColor" className={className} role="img" aria-label="Linux"><path d={LINUX_D} /></svg>
    )
  }
  // unknown OS → a generic server/box glyph
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.7} strokeLinecap="round" strokeLinejoin="round" className={className} role="img" aria-label="machine">
      <rect x="3" y="4" width="18" height="7" rx="1.5" /><rect x="3" y="13" width="18" height="7" rx="1.5" /><line x1="7" y1="7.5" x2="7" y2="7.5" /><line x1="7" y1="16.5" x2="7" y2="16.5" />
    </svg>
  )
}
