// Decorative SVG illustrations for the docs pages. Hand-drawn-ish line
// art in the landing's orchid-purple, no fill, organic bezier curves.
// Used as page-hero illustrations via {{illust:name}} markers in markdown.

interface IllustProps {
  width?: number
  height?: number
}

function frame(children: React.ReactNode, w = 560, h = 220) {
  return (
    <div className="docs-illust" style={{ display: 'flex', justifyContent: 'center', margin: '20px 0 32px' }}>
      <svg
        viewBox={`0 0 ${w} ${h}`} width="100%" style={{ maxWidth: w, height: 'auto' }}
        fill="none" stroke="#7c3aed" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"
      >
        {children}
      </svg>
    </div>
  )
}

// Single bud opening — a stem with a closed/opening orchid bud at the
// top, two leaves on the side. Conveys "starting fresh".
export function Bud() {
  return frame(<>
    {/* stem */}
    <path d="M280 200 C 278 160 282 120 280 80" />
    {/* leaves */}
    <path d="M280 175 C 240 170 215 158 200 138 C 225 142 255 154 280 168" />
    <path d="M280 155 C 320 150 345 138 360 118 C 335 122 305 134 280 148" />
    {/* bud, three layered petal arcs */}
    <path d="M280 80 C 256 78 244 60 256 44 C 268 32 292 32 304 44 C 316 60 304 78 280 80 Z" />
    <path d="M270 56 C 264 46 268 38 280 38 C 292 38 296 46 290 56" />
    <path d="M280 30 L 280 22" />
    {/* small accent dot */}
    <circle cx="280" cy="14" r="2.5" fill="#7c3aed" stroke="none" />
  </>)
}

// Three-flower spray — a small bouquet of stylized orchids fanning out.
// Used as the dashboard hero (each flower ~ a card).
export function Spray() {
  return frame(<>
    {/* stems converging from below */}
    <path d="M280 210 C 280 180 220 150 170 110" />
    <path d="M280 210 C 280 175 280 140 280 95" />
    <path d="M280 210 C 280 180 340 150 390 110" />
    {/* leaf at base */}
    <path d="M260 200 C 230 200 210 188 200 170 C 225 178 250 188 268 196" />
    <path d="M300 200 C 330 200 350 188 360 170 C 335 178 310 188 292 196" />
    {/* left flower */}
    <ellipse cx="170" cy="100" rx="14" ry="9" />
    <path d="M156 100 C 144 88 144 76 158 70 C 168 68 172 80 168 94" />
    <path d="M184 100 C 196 88 196 76 182 70 C 172 68 168 80 172 94" />
    <circle cx="170" cy="100" r="2" fill="#7c3aed" stroke="none" />
    {/* center flower (slightly bigger) */}
    <ellipse cx="280" cy="86" rx="16" ry="10" />
    <path d="M264 86 C 252 74 252 60 268 54 C 280 52 286 66 280 78" />
    <path d="M296 86 C 308 74 308 60 292 54 C 280 52 274 66 280 78" />
    <path d="M280 96 C 274 108 286 108 280 96" />
    <circle cx="280" cy="86" r="2.5" fill="#7c3aed" stroke="none" />
    {/* right flower */}
    <ellipse cx="390" cy="100" rx="14" ry="9" />
    <path d="M376 100 C 364 88 364 76 378 70 C 388 68 392 80 388 94" />
    <path d="M404 100 C 416 88 416 76 402 70 C 392 68 388 80 392 94" />
    <circle cx="390" cy="100" r="2" fill="#7c3aed" stroke="none" />
  </>)
}

// Padlock entwined with a vine — for the Security page.
export function LockVine() {
  return frame(<>
    {/* shackle */}
    <path d="M250 100 V 80 C 250 56 270 40 290 40 C 310 40 330 56 330 80 V 100" />
    {/* body */}
    <rect x="234" y="100" width="112" height="84" rx="10" />
    {/* keyhole */}
    <circle cx="290" cy="138" r="6" />
    <path d="M290 144 V 162" />
    {/* vine wrapping around */}
    <path d="M210 180 C 220 150 240 130 232 100 C 226 80 240 60 270 60 C 268 80 254 92 246 110" />
    <path d="M370 200 C 360 170 348 154 354 130 C 360 110 348 90 322 86" />
    {/* tiny leaves on the vine */}
    <path d="M222 130 C 210 126 204 134 210 142 C 218 142 224 138 222 130 Z" />
    <path d="M358 156 C 370 152 376 160 370 168 C 362 168 356 164 358 156 Z" />
    <path d="M252 76 C 244 70 236 76 240 84 C 248 86 254 82 252 76 Z" />
  </>)
}

// Woven vines — a tangled mesh of curves, four nodes glow.
// For the Tailscale page (mesh / tailnet visual).
export function VineMesh() {
  return frame(<>
    {/* mesh curves */}
    <path d="M80 60 C 200 30 360 200 480 60" />
    <path d="M80 160 C 200 200 360 30 480 160" />
    <path d="M80 110 C 180 80 380 140 480 110" />
    <path d="M120 30 C 240 100 320 130 440 200" />
    <path d="M120 200 C 240 130 320 100 440 30" />
    {/* node dots */}
    <circle cx="120" cy="60"  r="6" fill="#fafaf9" />
    <circle cx="120" cy="60"  r="3" fill="#7c3aed" stroke="none" />
    <circle cx="440" cy="60"  r="6" fill="#fafaf9" />
    <circle cx="440" cy="60"  r="3" fill="#7c3aed" stroke="none" />
    <circle cx="120" cy="160" r="6" fill="#fafaf9" />
    <circle cx="120" cy="160" r="3" fill="#7c3aed" stroke="none" />
    <circle cx="440" cy="160" r="6" fill="#fafaf9" />
    <circle cx="440" cy="160" r="3" fill="#7c3aed" stroke="none" />
    <circle cx="280" cy="110" r="6" fill="#fafaf9" />
    <circle cx="280" cy="110" r="3" fill="#7c3aed" stroke="none" />
  </>)
}

// Branching tree — single root that forks twice. Architecture page.
export function BranchTree() {
  return frame(<>
    {/* trunk */}
    <path d="M280 210 C 280 180 280 160 280 140" />
    {/* fork 1 */}
    <path d="M280 140 C 280 120 220 110 180 90" />
    <path d="M280 140 C 280 120 340 110 380 90" />
    {/* leaves on each branch end */}
    <path d="M180 90 C 160 80 150 64 162 50 C 176 46 188 60 188 76" />
    <path d="M380 90 C 400 80 410 64 398 50 C 384 46 372 60 372 76" />
    {/* mid sprouts */}
    <path d="M280 175 C 256 170 240 158 232 144" />
    <path d="M280 175 C 304 170 320 158 328 144" />
    {/* root accents */}
    <path d="M260 215 C 250 220 240 222 230 220" />
    <path d="M300 215 C 310 220 320 222 330 220" />
    <circle cx="280" cy="138" r="2.5" fill="#7c3aed" stroke="none" />
  </>)
}

// Chat bubble with a sprig of petals — for Supervision page.
export function ChatVine() {
  return frame(<>
    {/* bubble */}
    <path d="M160 70 C 160 50 180 40 210 40 H 360 C 390 40 410 50 410 70 V 130 C 410 150 390 160 360 160 H 230 L 200 184 V 160 H 210 C 180 160 160 150 160 130 Z" />
    {/* three dots */}
    <circle cx="240" cy="100" r="3" fill="#7c3aed" stroke="none" />
    <circle cx="285" cy="100" r="3" fill="#7c3aed" stroke="none" />
    <circle cx="330" cy="100" r="3" fill="#7c3aed" stroke="none" />
    {/* sprig coming off the top-right */}
    <path d="M390 50 C 410 30 430 24 446 32" />
    <path d="M420 30 C 416 18 426 14 432 22" />
    <path d="M440 36 C 440 24 452 22 456 32" />
  </>)
}

// Hand cradling a single bud — for Capture page.
export function HandBud() {
  return frame(<>
    {/* curved palm */}
    <path d="M180 160 C 200 200 320 220 380 188 C 400 178 408 160 396 150 C 350 130 240 130 196 142 C 184 146 180 152 180 160 Z" />
    {/* fingers (3 short arcs) */}
    <path d="M226 142 C 226 122 234 110 244 108" />
    <path d="M268 138 C 268 116 276 102 290 102" />
    <path d="M312 140 C 312 118 320 108 332 110" />
    {/* small bud floating above palm */}
    <ellipse cx="280" cy="74" rx="12" ry="9" />
    <path d="M280 60 V 50" />
    <path d="M268 74 C 256 64 256 50 270 46" />
    <path d="M292 74 C 304 64 304 50 290 46" />
    <circle cx="280" cy="74" r="2" fill="#7c3aed" stroke="none" />
  </>)
}

// Hexagon machine cluster — for VMs page.
export function VMCluster() {
  return frame(<>
    {/* center hex */}
    <polygon points="280,90 320,113 320,159 280,182 240,159 240,113" />
    {/* satellite hexes */}
    <polygon points="180,46 212,64 212,100 180,118 148,100 148,64" />
    <polygon points="380,46 412,64 412,100 380,118 348,100 348,64" />
    <polygon points="180,154 212,172 212,208 180,226 148,208 148,172" />
    <polygon points="380,154 412,172 412,208 380,226 348,208 348,172" />
    {/* connecting lines from center to satellites */}
    <path d="M260 102 L 210 90" />
    <path d="M300 102 L 350 90" />
    <path d="M260 170 L 210 188" />
    <path d="M300 170 L 350 188" />
    {/* dots = sessions running */}
    <circle cx="280" cy="130" r="3" fill="#7c3aed" stroke="none" />
    <circle cx="180" cy="82"  r="2.5" fill="#7c3aed" stroke="none" />
    <circle cx="380" cy="82"  r="2.5" fill="#7c3aed" stroke="none" />
    <circle cx="180" cy="190" r="2.5" fill="#7c3aed" stroke="none" />
    <circle cx="380" cy="190" r="2.5" fill="#7c3aed" stroke="none" />
  </>)
}

// Two arrows fanning to different blooms — Targets page.
export function ForkBlooms() {
  return frame(<>
    {/* origin */}
    <circle cx="100" cy="110" r="5" fill="#7c3aed" stroke="none" />
    {/* curves to three blooms */}
    <path d="M105 110 C 200 90 280 50 380 50" />
    <path d="M105 110 C 220 110 300 110 380 110" />
    <path d="M105 110 C 200 130 280 170 380 170" />
    {/* arrowheads */}
    <path d="M370 46 L 380 50 L 372 56" />
    <path d="M370 106 L 380 110 L 372 114" />
    <path d="M370 166 L 380 170 L 372 174" />
    {/* three bloom hints */}
    <ellipse cx="408" cy="50"  rx="14" ry="9" />
    <circle cx="408" cy="50"  r="2.5" fill="#7c3aed" stroke="none" />
    <ellipse cx="408" cy="110" rx="14" ry="9" />
    <circle cx="408" cy="110" r="2.5" fill="#7c3aed" stroke="none" />
    <ellipse cx="408" cy="170" rx="14" ry="9" />
    <circle cx="408" cy="170" r="2.5" fill="#7c3aed" stroke="none" />
  </>)
}

// Knot / config — interlocking loops. Configuration page.
export function ConfigKnot() {
  return frame(<>
    <path d="M200 70 C 160 70 160 150 200 150 C 240 150 240 70 280 70 C 320 70 320 150 360 150 C 400 150 400 70 360 70" />
    <path d="M200 70 C 240 70 240 150 280 150 C 320 150 320 70 360 70" />
    {/* small leaves on either end */}
    <path d="M180 100 C 168 96 162 104 168 112 C 176 112 182 108 180 100 Z" />
    <path d="M380 120 C 392 124 398 116 392 108 C 384 108 378 112 380 120 Z" />
  </>)
}

export const ILLUSTRATIONS: Record<string, React.FC<IllustProps>> = {
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
