import Foundation

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
    var source: String           // "macos" | "ios"
    var kind: DraftKind
    var note: String
    var image: DraftImage?
    var link: DraftLink?
    var text: DraftText?
    var context: DraftContext?
    var target: DraftTarget?

    static func make(
        kind: DraftKind,
        note: String,
        source: String = "macos",
        target: DraftTarget? = nil
    ) -> Draft {
        Draft(
            id: ulidLike(),
            createdAt: Date(),
            source: source,
            kind: kind,
            note: note,
            image: nil, link: nil, text: nil,
            context: nil,
            target: target ?? DraftTarget(repo: "denoland/orchid", labels: ["clawpatrol"])
        )
    }
}

/// Where a draft should land. The label drives orchid's swarm routing —
/// see `denoland/orchid` inbox label config.
enum CaptureRoute: String, CaseIterable, Identifiable {
    case clawpatrol
    case orchid
    case deno

    var id: String { rawValue }
    var label: String { rawValue }
    var displayName: String {
        switch self {
        case .clawpatrol: "clawpatrol"
        case .orchid: "orchid"
        case .deno: "deno"
        }
    }
    var draftTarget: DraftTarget {
        DraftTarget(repo: "denoland/orchid", labels: [label])
    }
}

/// A short, time-sortable ID. Not a real ULID — Foundation's UUID would do, but
/// this keeps drafts naturally ordered when you `ls` the queue.
func ulidLike() -> String {
    let ts = String(Int(Date().timeIntervalSince1970 * 1000), radix: 36).uppercased()
    let rand = (0..<8).map { _ in
        let chars = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
        return chars.randomElement()!
    }
    return ts + "-" + String(rand)
}
