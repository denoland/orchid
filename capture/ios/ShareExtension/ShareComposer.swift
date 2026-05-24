import SwiftUI

/// The mini composer that renders inside the share sheet. One line of context,
/// one Capture button. Matches the visual language of the main app.
struct ShareComposer: View {
    let initialDraft: Draft?
    let onSubmit: (Draft) -> Void
    let onCancel: () -> Void

    @State private var note: String = ""
    @State private var draft: Draft?

    init(initialDraft: Draft?,
         onSubmit: @escaping (Draft) -> Void,
         onCancel: @escaping () -> Void)
    {
        self.initialDraft = initialDraft
        _draft = State(initialValue: initialDraft)
        self.onSubmit = onSubmit
        self.onCancel = onCancel
    }

    var body: some View {
        NavigationStack {
            VStack(alignment: .leading, spacing: 14) {
                if let d = draft {
                    artifactRow(d)
                } else {
                    Text("Nothing to capture.")
                        .foregroundStyle(.secondary)
                }

                TextField("describe this in one sentence",
                          text: $note, axis: .vertical)
                    .lineLimit(3...6)
                    .font(.system(.body, design: .monospaced))
                    .padding(10)
                    .background(Color.black.opacity(0.04))
                    .cornerRadius(6)

                Spacer()
            }
            .padding(16)
            .navigationTitle("orchid")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .navigationBarLeading) {
                    Button("Cancel", action: onCancel)
                }
                ToolbarItem(placement: .navigationBarTrailing) {
                    Button("Capture") {
                        guard var d = draft else { return }
                        d.note = note
                        onSubmit(d)
                    }
                    .disabled(draft == nil)
                }
            }
        }
        .tint(.black)
    }

    @ViewBuilder
    private func artifactRow(_ d: Draft) -> some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: icon(for: d.kind))
                .frame(width: 28, height: 28)
                .background(Color.black.opacity(0.06))
            VStack(alignment: .leading, spacing: 2) {
                Text(label(for: d.kind))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Text(humanText(for: d))
                    .font(.system(.body, design: .monospaced))
                    .lineLimit(2)
                    .truncationMode(.middle)
            }
            Spacer()
        }
    }

    private func icon(for kind: DraftKind) -> String {
        switch kind {
        case .screenshot: return "photo"
        case .link: return "link"
        case .text: return "text.alignleft"
        case .voice: return "waveform"
        }
    }

    private func label(for kind: DraftKind) -> String {
        switch kind {
        case .screenshot: return "image"
        case .link: return "link"
        case .text: return "text"
        case .voice: return "voice"
        }
    }

    private func humanText(for d: Draft) -> String {
        if let l = d.link?.url { return URL(string: l)?.host ?? l }
        if let t = d.text?.body { return String(t.prefix(80)) }
        if d.image != nil { return "image attached" }
        return ""
    }
}
