import UIKit
import SwiftUI
import UniformTypeIdentifiers
import Social

/// The system share sheet hosts this controller when the user shares text,
/// a URL, an image, or a screenshot to OrchidCapture. We inspect the
/// `NSExtensionContext` attachments, materialise the right `Draft` shape,
/// and present a minimal black-and-white SwiftUI confirmation screen.
final class ShareViewController: UIViewController {
    private var hosting: UIHostingController<ShareComposer>?

    override func viewDidLoad() {
        super.viewDidLoad()
        view.backgroundColor = .systemBackground

        Task { @MainActor in
            let draft = await buildDraft(from: extensionContext)
            present(draft: draft)
        }
    }

    @MainActor
    private func present(draft: Draft?) {
        let composer = ShareComposer(
            initialDraft: draft,
            onSubmit: { [weak self] toSend in
                Task { @MainActor in
                    let store = await ShareDraftSubmitter()
                    await store.send(toSend)
                    self?.completeRequest()
                }
            },
            onCancel: { [weak self] in self?.cancelRequest() }
        )
        let host = UIHostingController(rootView: composer)
        host.view.frame = view.bounds
        host.view.autoresizingMask = [.flexibleWidth, .flexibleHeight]
        addChild(host)
        view.addSubview(host.view)
        host.didMove(toParent: self)
        hosting = host
    }

    private func completeRequest() {
        extensionContext?.completeRequest(returningItems: nil, completionHandler: nil)
    }

    private func cancelRequest() {
        let err = NSError(domain: "OrchidCapture", code: 0,
                          userInfo: [NSLocalizedDescriptionKey: "user cancelled"])
        extensionContext?.cancelRequest(withError: err)
    }
}

/// Walks the share sheet's attachments and produces a `Draft`. Prefers the
/// richest single attachment: image > URL > text. Multi-attachment items
/// take the first hit of each kind.
@MainActor
func buildDraft(from ctx: NSExtensionContext?) async -> Draft? {
    guard let items = ctx?.inputItems as? [NSExtensionItem] else { return nil }

    for item in items {
        for provider in item.attachments ?? [] {
            if provider.hasItemConformingToTypeIdentifier(UTType.image.identifier) {
                if let (data, mime) = try? await loadImage(provider) {
                    var draft = Draft(
                        id: ulidLike(),
                        createdAt: Date(),
                        source: "ios-share",
                        kind: .screenshot,
                        note: "",
                        voice: nil,
                        target: DraftTarget(repo: "denoland/orchid", labels: nil)
                    )
                    // The share extension also fills in the image slot.
                    // (Draft uses DraftVoice for ios; we add an inline image
                    // helper to keep the cross-platform schema aligned.)
                    let inline = InlineImage(mime: mime, bytesBase64: data.base64EncodedString())
                    return draft.with(image: inline)
                }
            }
            if provider.hasItemConformingToTypeIdentifier(UTType.url.identifier) {
                if let url = try? await loadURL(provider) {
                    var draft = Draft(
                        id: ulidLike(), createdAt: Date(),
                        source: "ios-share", kind: .link, note: "",
                        voice: nil,
                        target: DraftTarget(repo: "denoland/orchid", labels: nil)
                    )
                    return draft.with(link: DraftLink(url: url.absoluteString, title: nil))
                }
            }
            if provider.hasItemConformingToTypeIdentifier(UTType.plainText.identifier) {
                if let s = try? await loadText(provider) {
                    var draft = Draft(
                        id: ulidLike(), createdAt: Date(),
                        source: "ios-share", kind: .text, note: "",
                        voice: nil,
                        target: DraftTarget(repo: "denoland/orchid", labels: nil)
                    )
                    return draft.with(text: DraftText(body: s, originURL: nil))
                }
            }
        }
    }
    return nil
}

private func loadImage(_ provider: NSItemProvider) async throws -> (Data, String) {
    let item = try await provider.loadItem(forTypeIdentifier: UTType.image.identifier, options: nil)
    if let url = item as? URL, let data = try? Data(contentsOf: url) {
        return (data, "image/png")
    }
    if let img = item as? UIImage, let data = img.pngData() {
        return (data, "image/png")
    }
    if let data = item as? Data {
        return (data, "image/png")
    }
    throw NSError(domain: "OrchidCapture", code: 10)
}

private func loadURL(_ provider: NSItemProvider) async throws -> URL {
    let item = try await provider.loadItem(forTypeIdentifier: UTType.url.identifier, options: nil)
    if let url = item as? URL { return url }
    if let s = item as? String, let url = URL(string: s) { return url }
    throw NSError(domain: "OrchidCapture", code: 11)
}

private func loadText(_ provider: NSItemProvider) async throws -> String {
    let item = try await provider.loadItem(forTypeIdentifier: UTType.plainText.identifier, options: nil)
    if let s = item as? String { return s }
    if let url = item as? URL, let data = try? Data(contentsOf: url) {
        return String(data: data, encoding: .utf8) ?? ""
    }
    throw NSError(domain: "OrchidCapture", code: 12)
}
