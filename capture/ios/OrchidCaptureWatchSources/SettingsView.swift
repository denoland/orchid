import SwiftUI

/// Minimal settings sheet. Surfaces the values that landed via
/// WatchConnectivity / env-var fallback and exposes "Retry queued" so
/// the user can drain anything that piled up offline.
///
/// We deliberately don't expose an endpoint text-input here — typing
/// URLs on a watch is hostile. The pairing flow is "open the iPhone
/// app once, it pushes the values over." See WatchSettings.swift.
struct SettingsView: View {
    @EnvironmentObject var settings: WatchSettings
    @EnvironmentObject var store:    DraftStore
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                row(label: "Endpoint",
                    value: settings.endpoint?.host ?? "—",
                    secondary: settings.endpoint?.absoluteString)
                row(label: "Token",
                    value: settings.token.isEmpty ? "—" : maskToken(settings.token))
                row(label: "Synced from iPhone",
                    value: lastSyncString())

                Divider().padding(.vertical, 4)

                Button {
                    Task { await store.drainQueue() }
                } label: {
                    HStack {
                        Image(systemName: "arrow.up.circle")
                        Text("Retry queued (\(store.pending))")
                    }
                    .frame(maxWidth: .infinity)
                }
                .buttonStyle(.bordered)
                .disabled(store.pending == 0 || !settings.isConfigured || store.inFlight)

                Button(role: .destructive) {
                    store.clearQueue()
                } label: {
                    HStack {
                        Image(systemName: "trash")
                        Text("Clear queue")
                    }
                    .frame(maxWidth: .infinity)
                }
                .buttonStyle(.bordered)
                .disabled(store.pending == 0)

                Text("Open the iPhone app and toggle Sync to Watch to push your endpoint and token. The watch will retry any captures that piled up while offline.")
                    .font(.system(size: 10))
                    .foregroundStyle(.secondary)
                    .padding(.top, 8)
            }
            .padding(.horizontal, 10)
        }
        .toolbar {
            ToolbarItem(placement: .cancellationAction) {
                Button("Done") { dismiss() }
            }
        }
    }

    private func row(label: String, value: String, secondary: String? = nil) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(label)
                .font(.system(size: 10, weight: .semibold))
                .foregroundStyle(.secondary)
            Text(value)
                .font(.system(size: 13, weight: .medium))
                .lineLimit(1)
                .truncationMode(.middle)
            if let secondary, secondary != value {
                Text(secondary)
                    .font(.system(size: 9))
                    .foregroundStyle(.secondary.opacity(0.7))
                    .lineLimit(1)
                    .truncationMode(.middle)
            }
        }
    }

    private func lastSyncString() -> String {
        guard let ts = settings.lastSyncFromPhone else { return "never" }
        let f = RelativeDateTimeFormatter()
        f.unitsStyle = .short
        return f.localizedString(for: ts, relativeTo: Date())
    }

    private func maskToken(_ t: String) -> String {
        if t.count <= 8 { return String(repeating: "•", count: t.count) }
        let tail = t.suffix(4)
        return String(repeating: "•", count: t.count - 4) + tail
    }
}

#Preview {
    let settings = WatchSettings()
    let store = DraftStore(settings: settings)
    return SettingsView()
        .environmentObject(settings)
        .environmentObject(store)
}
