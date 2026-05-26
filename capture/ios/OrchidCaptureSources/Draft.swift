import Foundation

enum DraftKind: String, Codable {
    case screenshot, link, text, voice
}

struct InlineImage: Codable {
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
    var source: String
    var kind: DraftKind
    var note: String
    var image: InlineImage?
    var link: DraftLink?
    var text: DraftText?
    var voice: DraftVoice?
    var context: DraftContext?
    var target: DraftTarget?

    init(
        id: String,
        createdAt: Date,
        source: String,
        kind: DraftKind,
        note: String,
        image: InlineImage? = nil,
        link: DraftLink? = nil,
        text: DraftText? = nil,
        voice: DraftVoice? = nil,
        context: DraftContext? = nil,
        target: DraftTarget? = nil
    ) {
        self.id = id
        self.createdAt = createdAt
        self.source = source
        self.kind = kind
        self.note = note
        self.image = image
        self.link = link
        self.text = text
        self.voice = voice
        self.context = context
        self.target = target
    }

    func with(image: InlineImage) -> Draft { var d = self; d.image = image; return d }
    func with(link: DraftLink) -> Draft { var d = self; d.link = link; return d }
    func with(text: DraftText) -> Draft { var d = self; d.text = text; return d }
    func with(voice: DraftVoice) -> Draft { var d = self; d.voice = voice; return d }
}

func ulidLike() -> String {
    let ts = String(Int(Date().timeIntervalSince1970 * 1000), radix: 36).uppercased()
    let chars = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
    let rand = (0..<8).map { _ in chars.randomElement()! }
    return ts + "-" + String(rand)
}
