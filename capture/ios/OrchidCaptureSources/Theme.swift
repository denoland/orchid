import SwiftUI
import UIKit

/// Design tokens ported from the web dashboard (www/src/index.css + the
/// landing DashFrame mockup). Black-and-white surface, serif wordmark,
/// mono meta, and the four status colors that carry all the signal:
///   needs-you → rose · watching → amber · working → emerald · quiet → zinc
///
/// Everything is theme-aware (light/dark) via dynamic UIColor providers so
/// the app follows the system appearance the way the dashboard follows
/// `html.dark`.
enum Theme {
    // ── surface + text ────────────────────────────────────────────────
    static let surface   = dyn(light: 0xF7F7F8, dark: 0x0A0A0B)
    static let panel     = dyn(light: 0xFFFFFF, dark: 0x18181B)
    static let panelSunk = dyn(light: 0xFAFAFA, dark: 0x121214)
    static let ink       = dyn(light: 0x18181B, dark: 0xE4E4E7)
    static let muted     = dyn(light: 0x71717A, dark: 0xA1A1AA)
    static let faint     = dyn(light: 0xA1A1AA, dark: 0x52525B)
    static let line      = dyn(light: 0xE4E4E7, dark: 0x27272A)

    // ── accent + status ───────────────────────────────────────────────
    static let orchid  = dyn(light: 0x7C3AED, dark: 0xA78BFA)
    static let rose    = dyn(light: 0xF43F5E, dark: 0xFB7185) // needs-you / CI fail
    static let amber   = dyn(light: 0xF59E0B, dark: 0xFBBF24) // watching
    static let emerald = dyn(light: 0x10B981, dark: 0x34D399) // working / CI pass / PR open
    static let zinc    = dyn(light: 0xD4D4D8, dark: 0x52525B) // quiet
    static let sky     = dyn(light: 0x0EA5E9, dark: 0x38BDF8) // building, no PR yet
    static let violet  = dyn(light: 0x8B5CF6, dark: 0xA78BFA) // merged
    static let claudeMark = dyn(light: 0xCC7C5A, dark: 0xCC7C5A)

    // chrome
    static let navBg   = dyn(light: 0xFFFFFF, dark: 0x09090B)
    static let chipBg  = dyn(light: 0xE5E5EA, dark: 0x3F3F46)
    static let searchBg = dyn(light: 0xF1F1F3, dark: 0x27272A)

    // ── type ──────────────────────────────────────────────────────────
    /// The "Orchid" wordmark — Cormorant on web; system serif italic here.
    static func wordmark(_ size: CGFloat) -> Font {
        .system(size: size, weight: .medium, design: .serif).italic()
    }
    static func serif(_ size: CGFloat, weight: Font.Weight = .regular) -> Font {
        .system(size: size, weight: weight, design: .serif)
    }
    static func mono(_ size: CGFloat, weight: Font.Weight = .regular) -> Font {
        .system(size: size, weight: weight, design: .monospaced)
    }

    // ── helpers ────────────────────────────────────────────────────────
    static func dyn(light: Int, dark: Int) -> Color {
        Color(UIColor { tc in
            tc.userInterfaceStyle == .dark ? rgb(dark) : rgb(light)
        })
    }
    private static func rgb(_ hex: Int) -> UIColor {
        UIColor(
            red:   CGFloat((hex >> 16) & 0xFF) / 255,
            green: CGFloat((hex >> 8) & 0xFF) / 255,
            blue:  CGFloat(hex & 0xFF) / 255,
            alpha: 1
        )
    }
}
