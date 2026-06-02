import SwiftUI

/// Sessions — the dashboard list view. Tight, single-line rows: a state
/// glyph, the issue title, a `agent · repo · #issue · age` meta line, and
/// a trailing CI/attention marker. needs-you rows get a rose left rule +
/// faint tint (matching the web .dmn-row.needs). Live state from StateStore.
struct JobsView: View {
    @EnvironmentObject private var store: StateStore
    @AppStorage("orchid.endpoint") private var endpoint: String = ""
    @State private var query = ""

    private var jobs: [Job] {
        (store.state.jobs ?? []).filter { !$0.isClosed }
    }

    private var filtered: [Job] {
        guard !query.isEmpty else { return jobs }
        let q = query.lowercased()
        return jobs.filter {
            ($0.issueTitle ?? "").lowercased().contains(q)
            || $0.repoLabel.lowercased().contains(q)
            || "\($0.issue)".contains(q)
        }
    }

    private var groups: [(AttentionLevel, [Job])] {
        let by = Dictionary(grouping: filtered) { attention($0).level }
        return by.keys.sorted { $0.order < $1.order }
            .map { ($0, by[$0]!.sorted { attention($0).score > attention($1).score }) }
    }

    var body: some View {
        NavigationStack {
            Group {
                if endpoint.isEmpty {
                    Hint(icon: "gearshape", title: "Add your endpoint",
                         detail: "Settings → endpoint + dashboard token.")
                } else if jobs.isEmpty {
                    Hint(icon: store.lastError == nil ? "tray" : "wifi.exclamationmark",
                         title: store.lastError == nil ? "No active sessions" : "Can’t reach orchid",
                         detail: store.lastError ?? "File an inbox issue to spawn one.")
                } else {
                    list
                }
            }
            .background(Theme.surface)
            .navigationTitle("Sessions")
            .navigationBarTitleDisplayMode(.large)
            .toolbar { ToolbarItem(placement: .topBarTrailing) { ConnDot(link: store.link) } }
        }
        .onAppear { store.start() }
    }

    private var list: some View {
        List {
            ForEach(groups, id: \.0) { level, items in
                Section {
                    ForEach(items) { SessionRow(job: $0) }
                } header: {
                    Text(level.title.uppercased())
                        .font(Theme.mono(10, weight: .semibold)).tracking(1.4)
                        .foregroundStyle(Theme.muted)
                }
            }
        }
        .listStyle(.plain)
        .scrollContentBackground(.hidden)
        .background(Theme.surface)
        .searchable(text: $query, prompt: "Search sessions")
        .refreshable { store.reconnect() }
    }
}

// ─── row ──────────────────────────────────────────────────────────────────

struct SessionRow: View {
    let job: Job
    private var att: Attention { attention(job) }
    private var needs: Bool { att.level == .needsYou }

    var body: some View {
        let content = HStack(spacing: 11) {
            StateGlyph(job: job, level: att.level).frame(width: 16)
            VStack(alignment: .leading, spacing: 2) {
                Text(job.issueTitle?.isEmpty == false ? job.issueTitle! : "—")
                    .font(.system(size: 14, weight: needs ? .semibold : .regular))
                    .foregroundStyle(Theme.ink)
                    .lineLimit(1)
                Text(meta).font(Theme.mono(11)).foregroundStyle(Theme.faint).lineLimit(1)
            }
            Spacer(minLength: 8)
            Trailing(job: job, level: att.level)
        }
        .padding(.vertical, 9)
        .padding(.trailing, 16)
        .padding(.leading, 16)

        return Group {
            if job.tmuxName.isEmpty {
                content
            } else {
                NavigationLink {
                    PaneView(tmux: job.tmuxName, title: "\(job.repoLabel) · #\(job.issue)")
                } label: { content }
            }
        }
        .listRowInsets(EdgeInsets())
        .listRowBackground(
            ZStack(alignment: .leading) {
                (needs ? Theme.rose.opacity(0.06) : Color.clear)
                if needs { Rectangle().fill(Theme.rose).frame(width: 3) }
            }
        )
    }

    private var meta: String {
        let agent = (job.vm ?? "").contains("codex") ? "codex" : "claude"
        var parts = [agent, job.repoLabel, "#\(job.issue)"]
        if job.prNum > 0 { parts.append("PR #\(job.prNum)") }
        let age = relAge(job.spawnedAt)
        if !age.isEmpty { parts.append(age) }
        return parts.joined(separator: "  ·  ")
    }
}

/// Leading state glyph — all SF Symbols. Open PR → green pull-request mark;
/// active-without-PR → spinner; otherwise a level-colored dot.
private struct StateGlyph: View {
    let job: Job
    let level: AttentionLevel
    var body: some View {
        if job.prNum > 0 {
            Image(systemName: "arrow.triangle.pull")
                .font(.system(size: 13, weight: .semibold))
                .foregroundStyle(Theme.emerald)
        } else if level == .working {
            ProgressView().controlSize(.mini).tint(Theme.amber)
        } else {
            Image(systemName: "circle.fill")
                .font(.system(size: 8))
                .foregroundStyle(level.color)
        }
    }
}

/// Trailing marker — SF Symbol CI status, or a needs-you pill.
private struct Trailing: View {
    let job: Job
    let level: AttentionLevel
    var body: some View {
        switch level {
        case .needsYou:
            Text("needs you")
                .font(Theme.mono(10, weight: .semibold))
                .foregroundStyle(.white)
                .padding(.horizontal, 8).padding(.vertical, 3)
                .background(Theme.rose, in: Capsule())
        case .watching:
            switch ciStatus(job.lastCheckConclusions) {
            case .pass: Image(systemName: "checkmark.circle.fill").foregroundStyle(Theme.emerald)
            case .fail: Image(systemName: "xmark.circle.fill").foregroundStyle(Theme.rose)
            default:    Image(systemName: "clock").foregroundStyle(Theme.amber)
            }
        case .working, .quiet:
            EmptyView()
        }
    }
}

// ─── shared chrome ──────────────────────────────────────────────────────────

struct ConnDot: View {
    let link: StateStore.Link
    var body: some View {
        let (c, label): (Color, String) = {
            switch link {
            case .live:    return (Theme.emerald, "live")
            case .polling: return (Theme.amber, "sync")
            case .offline: return (Theme.faint, "offline")
            }
        }()
        return HStack(spacing: 5) {
            Circle().fill(c).frame(width: 6, height: 6)
            Text(label).font(Theme.mono(10)).foregroundStyle(Theme.muted)
        }
    }
}

struct Hint: View {
    let icon: String
    let title: String
    let detail: String
    var body: some View {
        VStack(spacing: 9) {
            Image(systemName: icon).font(.system(size: 34, weight: .light))
                .foregroundStyle(Theme.faint)
            Text(title).font(.system(size: 15, weight: .medium)).foregroundStyle(Theme.ink)
            Text(detail).font(.footnote).foregroundStyle(Theme.muted)
                .multilineTextAlignment(.center)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .padding(40)
        .background(Theme.surface)
    }
}

extension AttentionLevel {
    var color: Color {
        switch self {
        case .needsYou: return Theme.rose
        case .watching: return Theme.amber
        case .working:  return Theme.emerald
        case .quiet:    return Theme.zinc
        }
    }
}
