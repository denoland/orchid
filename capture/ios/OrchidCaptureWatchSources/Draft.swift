import Foundation

/// Draft model used by the watchOS app. Intentionally identical in wire
/// shape to the macOS/iOS Draft (capture/macos/.../Draft.swift) so the
/// orch `/api/drafts` handler in capture_api.go decodes a watch draft
/// with the existing DraftPayload type — no server change needed.
///
/// The watch only emits `voice` and `text` drafts; the type-specific
/// payloads stay optional and nil for everything we don't carry.

enum DraftKind: String, Codable {
    case screenshot
    case link
    case text
    case voice
}

struct DraftImage: Codable {
    var mime: String
    var bytesBase64: String

    enum CodingKeys: String, CodingKey {
        case mime
        case bytesBase64 = "bytes_base64"
    }
}

struct DraftLink: Codable {
    var url: String
    var title: String?
}

struct DraftText: Codable {
    var body: String
    var originURL: String?
}

struct DraftVoice: Codable {
    var mime: String
    var bytesBase64: String
    var durationSec: Double

    enum CodingKeys: String, CodingKey {
        case mime
        case bytesBase64 = "bytes_base64"
        case durationSec
    }
}

struct DraftContext: Codable {
    var appName: String?
    var windowTitle: String?
    var selection: String?
}

struct DraftTarget: Codable {
    var repo: String?
    var labels: [String]?
}

struct Draft: Codable, Identifiable {
    var id: String
    var createdAt: Date
    var source: String           // "watchos" for everything from here
    var kind: DraftKind
    var note: String
    var image: DraftImage?
    var link: DraftLink?
    var text: DraftText?
    var voice: DraftVoice?
    var context: DraftContext?
    var target: DraftTarget?

    static func voice(
        note: String,
        audio: Data,
        mime: String,
        durationSec: Double,
        target: DraftTarget? = nil
    ) -> Draft {
        Draft(
            id: ulidLike(),
            createdAt: Date(),
            source: "watchos",
            kind: .voice,
            note: note,
            image: nil,
            link: nil,
            text: nil,
            voice: DraftVoice(
                mime: mime,
                bytesBase64: audio.base64EncodedString(),
                durationSec: durationSec
            ),
            context: nil,
            target: target ?? DraftTarget(
                repo: "denoland/orchid",
                labels: ["clawpatrol"]
            )
        )
    }

    static func text(
        body: String,
        target: DraftTarget? = nil
    ) -> Draft {
        Draft(
            id: ulidLike(),
            createdAt: Date(),
            source: "watchos",
            kind: .text,
            note: body,
            image: nil,
            link: nil,
            text: DraftText(body: body, originURL: nil),
            voice: nil,
            context: nil,
            target: target ?? DraftTarget(
                repo: "denoland/orchid",
                labels: ["clawpatrol"]
            )
        )
    }
}

/// Same id scheme as the macOS app: millisecond timestamp in base-36 +
/// 8 random chars. Naturally lexicographically sortable.
func ulidLike() -> String {
    let ts = String(Int(Date().timeIntervalSince1970 * 1000),
                    radix: 36).uppercased()
    let chars = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
    let rand = (0..<8).map { _ in chars.randomElement()! }
    return ts + "-" + String(rand)
}
