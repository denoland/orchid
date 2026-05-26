// Tiny line-art icons drawn with explicit symmetric geometry. Used as
// page-identity badges — small, consistent stroke width, no wobble.

function frame(children: React.ReactNode, size = 96) {
  return (
    <div className="docs-illust" style={{ display: 'flex', justifyContent: 'flex-start', margin: '8px 0 16px' }}>
      <svg
        viewBox="-50 -50 100 100" width={size} height={size}
        fill="none" stroke="#7c3aed" strokeWidth="1.6"
        strokeLinecap="round" strokeLinejoin="round"
      >
        {children}
      </svg>
    </div>
  )
}

// Bud — single tulip-like bud on a stem with two leaves.
export function Bud() {
  return frame(<>
    <line x1="0" y1="-12" x2="0" y2="36" />
    <path d="M 0 14 Q -18 10 -28 -2" />
    <path d="M 0 14 Q  18 10  28 -2" />
    <path d="M 0 -34 Q -12 -28 -10 -16 Q -6 -10 0 -10 Q 6 -10 10 -16 Q 12 -28 0 -34 Z" />
    <line x1="0" y1="-36" x2="0" y2="-40" />
  </>)
}

// Three buds — small bouquet for the dashboard page.
export function Spray() {
  return frame(<>
    <line x1="0" y1="40" x2="0" y2="-12" />
    <path d="M 0 40 Q -16 24 -24 0" />
    <path d="M 0 40 Q  16 24  24 0" />
    <ellipse cx="-24" cy="-6" rx="5" ry="7" />
    <ellipse cx=" 24" cy="-6" rx="5" ry="7" />
    <ellipse cx="0"  cy="-22" rx="6" ry="9" />
  </>)
}

// Padlock — security page.
export function LockVine() {
  return frame(<>
    <path d="M -16 -4 V -16 Q -16 -32 0 -32 Q 16 -32 16 -16 V -4" />
    <rect x="-22" y="-4" width="44" height="34" rx="4" />
    <circle cx="0" cy="10" r="3" />
    <line x1="0" y1="13" x2="0" y2="22" />
  </>)
}

// Mesh — five nodes star-connected, for the tailnet page.
export function VineMesh() {
  const pts: [number, number][] = [
    [0, -28], [27, -8], [17, 24], [-17, 24], [-27, -8],
  ]
  const lines: React.ReactElement[] = []
  for (let i = 0; i < pts.length; i++) {
    for (let j = i + 1; j < pts.length; j++) {
      lines.push(
        <line key={`${i}-${j}`} x1={pts[i][0]} y1={pts[i][1]} x2={pts[j][0]} y2={pts[j][1]} />
      )
    }
  }
  return frame(<>
    {lines}
    {pts.map(([x, y], i) => (
      <circle key={i} cx={x} cy={y} r="3.5" fill="#fafaf9" />
    ))}
  </>)
}

// Branch — center trunk with two clean forks. Architecture.
export function BranchTree() {
  return frame(<>
    <line x1="0" y1="40" x2="0" y2="0" />
    <line x1="0" y1="0" x2="-26" y2="-20" />
    <line x1="0" y1="0" x2=" 26" y2="-20" />
    <circle cx="0"  cy="0"    r="2.5" fill="#7c3aed" stroke="none" />
    <circle cx="-26" cy="-20" r="3.5" />
    <circle cx=" 26" cy="-20" r="3.5" />
    <line x1="0" y1="16" x2="-14" y2="6" />
    <line x1="0" y1="16" x2=" 14" y2="6" />
  </>)
}

// Chat bubble with three dots — supervision.
export function ChatVine() {
  return frame(<>
    <path d="M -32 -16 H 32 Q 36 -16 36 -12 V 12 Q 36 16 32 16 H -10 L -20 26 V 16 H -32 Q -36 16 -36 12 V -12 Q -36 -16 -32 -16 Z" />
    <circle cx="-12" cy="0" r="2" fill="#7c3aed" stroke="none" />
    <circle cx="  0" cy="0" r="2" fill="#7c3aed" stroke="none" />
    <circle cx=" 12" cy="0" r="2" fill="#7c3aed" stroke="none" />
  </>)
}

// Pen nib — capture.
export function HandBud() {
  return frame(<>
    <path d="M -20 30 L 20 -30 L 28 -22 L -12 38 Z" />
    <line x1="-20" y1="30" x2="-8" y2="34" />
    <circle cx="-22" cy="40" r="2" fill="#7c3aed" stroke="none" />
  </>)
}

// Stack of three machines — VMs.
export function VMCluster() {
  return frame(<>
    <rect x="-30" y="-28" width="60" height="14" rx="2" />
    <rect x="-30" y=" -7" width="60" height="14" rx="2" />
    <rect x="-30" y=" 14" width="60" height="14" rx="2" />
    <circle cx="-22" cy="-21" r="1.5" fill="#7c3aed" stroke="none" />
    <circle cx="-22" cy="  0" r="1.5" fill="#7c3aed" stroke="none" />
    <circle cx="-22" cy=" 21" r="1.5" fill="#7c3aed" stroke="none" />
  </>)
}

// Fork — single trunk splitting into three. Targets.
export function ForkBlooms() {
  return frame(<>
    <line x1="-36" y1="0" x2="-4" y2="0" />
    <line x1="-4" y1="0" x2=" 28" y2="-24" />
    <line x1="-4" y1="0" x2=" 28" y2="  0" />
    <line x1="-4" y1="0" x2=" 28" y2=" 24" />
    <circle cx="-4" cy="0" r="2.5" fill="#7c3aed" stroke="none" />
    <circle cx="32" cy="-24" r="4" />
    <circle cx="32" cy="  0" r="4" />
    <circle cx="32" cy=" 24" r="4" />
  </>)
}

// Gear — configuration.
export function ConfigKnot() {
  const teeth = 8
  const inner = 16, outer = 26
  const pts: string[] = []
  for (let i = 0; i < teeth * 2; i++) {
    const ang = (i / (teeth * 2)) * Math.PI * 2 - Math.PI / 2
    const r = i % 2 === 0 ? outer : inner
    pts.push(`${(Math.cos(ang) * r).toFixed(1)} ${(Math.sin(ang) * r).toFixed(1)}`)
  }
  return frame(<>
    <polygon points={pts.join(' ')} />
    <circle cx="0" cy="0" r="8" />
  </>)
}

export const ILLUSTRATIONS: Record<string, React.FC> = {
  bud: Bud,
  spray: Spray,
  'lock-vine': LockVine,
  'vine-mesh': VineMesh,
  'branch-tree': BranchTree,
  'chat-vine': ChatVine,
  'hand-bud': HandBud,
  'vm-cluster': VMCluster,
  'fork-blooms': ForkBlooms,
  'config-knot': ConfigKnot,
}
