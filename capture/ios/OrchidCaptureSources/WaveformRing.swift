import SwiftUI

/// A circular waveform — N spokes around a center circle whose length tracks a
/// rolling buffer of recent input levels. The newest sample lives at the top
/// (12 o'clock) and walks counter-clockwise.
struct WaveformRing: View {
    /// History of normalized levels (0...1). New values appended.
    var levels: [Double]
    /// Inner radius (the recording circle).
    var inner: CGFloat = 110
    /// Max spoke length added on top of the inner radius.
    var spread: CGFloat = 60

    var body: some View {
        Canvas { ctx, size in drawSpokes(ctx: ctx, size: size) }
            .allowsHitTesting(false)
    }

    private func drawSpokes(ctx: GraphicsContext, size: CGSize) {
        let center = CGPoint(x: size.width / 2, y: size.height / 2)
        let n: Double = Double(max(levels.count, 60))
        for (i, raw) in levels.enumerated() {
            let v = CGFloat(max(0, min(1, raw)))
            let twoPi: Double = .pi * 2
            let angle: Double = (twoPi * Double(i) / n) - (.pi / 2)
            let cosA = CGFloat(cos(angle))
            let sinA = CGFloat(sin(angle))
            let r0: CGFloat = inner
            let r1: CGFloat = inner + 8 + (spread - 8) * v
            let p0 = CGPoint(x: center.x + cosA * r0, y: center.y + sinA * r0)
            let p1 = CGPoint(x: center.x + cosA * r1, y: center.y + sinA * r1)
            var path = Path()
            path.move(to: p0)
            path.addLine(to: p1)
            ctx.stroke(path, with: .color(.black), lineWidth: 2)
        }
    }
}
