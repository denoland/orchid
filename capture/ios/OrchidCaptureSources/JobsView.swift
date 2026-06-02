import SwiftUI

/// Sessions — the GitHub-app-style list that mirrors the web dashboard's
/// list view + the landing DashFrame mockup. Rows show a PR/CI glyph, the
/// issue title, a `agent · repo · #issue · age` meta line, and a trailing
/// status chip. Grouped by attention level. Live state comes from the
/// shared StateStore (WS + poll).
struct JobsView: View {
    @EnvironmentObject private var store: StateStore
    @AppStorage("orchid.endpoint") private var endpoint: String = ""
    @State private var query = ""

    private var jobs: [Job] {
        // The closed_jobs archive rides along in /api/state — keep it out
        // of the live list (that was the old "merged sessions linger" bug).
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
        return by.keys
            .sorted { $0.order < $1.order }
            .map { ($0, by[$0]!.sorted { attention($0).score > attention($1).score }) }
    }

    var body: some View {
        NavigationStack {
            Group {
                if endpoint.isEmpty {
                    SetupCard()
                } else if jobs.isEmpty {
                    EmptyState(error: store.lastError)
                } else {
                    list
                }
            }
            .background(Theme.surface.ignoresSafeArea())
            .navigationTitle("")
            .toolbar { toolbar }
            .toolbarBackground(Theme.surface, for: .navigationBar)
        }
        .tint(Theme.orchid)
        .onAppear { store.start() }
    }

    // ── list ────────────────────────────────────────────────────────
    private var list: some View {
        List {
            ForEach(groups, id: \.0) { level, items in
                Section {
                    ForEach(items) { job in row(job) }
                } header: {
                    SectionHeader(title: level.title, count: items.count, color: level.color)
                }
            }
            Color.clear.frame(height: 8).listRowBackground(Color.clear)
        }
        .listStyle(.plain)
        .scrollContentBackground(.hidden)
        .background(Theme.surface)
        .searchable(text: $query, placement: .navigationBarDrawer(displayMode: .automatic), prompt: "Search sessions")
        .refreshable { store.reconnect() }
    }

    @ViewBuilder
    private func row(_ job: Job) -> some View {
        if job.tmuxName.isEmpty {
            SessionRow(job: job).listRowBackground(Theme.surface)
        } else {
            NavigationLink {
                PaneView(tmux: job.tmuxName, title: "\(job.repoLabel) · #\(job.issue)")
            } label: {
                SessionRow(job: job)
            }
            .listRowBackground(Theme.surface)
        }
    }

    // ── toolbar (wordmark + live indicator) ───────────────────────────
    @ToolbarContentBuilder
    private var toolbar: some ToolbarContent {
        ToolbarItem(placement: .topBarLeading) {
            HStack(spacing: 8) {
                Text("Orchid").font(Theme.wordmark(28)).foregroundStyle(Theme.ink)
                if !jobs.isEmpty {
                    Text("\(jobs.count)").font(Theme.mono(12)).foregroundStyle(Theme.muted)
                }
            }
        }
        ToolbarItem(placement: .topBarTrailing) { ConnDot(link: store.link) }
    }
}

// ─── row ────────────────────────────────────────────────────────────────

struct SessionRow: View {
    let job: Job
    private var att: Attention { attention(job) }

    var body: some View {
        HStack(alignment: .top, spacing: 11) {
            PRGlyph(job: job, level: att.level)
                .frame(width: 18)
                .padding(.top, 1)

            VStack(alignment: .leading, spacing: 3) {
                Text(job.issueTitle?.isEmpty == false ? job.issueTitle! : "—")
                    .font(.system(size: 15))
                    .foregroundStyle(Theme.ink)
                    .lineLimit(2)
                MetaLine(job: job)
            }
            Spacer(minLength: 8)
            StatusChip(job: job, att: att)
        }
        .padding(.vertical, 5)
        .contentShape(Rectangle())
    }
}

private struct MetaLine: View {
    let job: Job
    var body: some View {
        let agent = (job.vm ?? "").contains("codex") ? "codex" : "claude"
        let age = relAge(job.spawnedAt)
        HStack(spacing: 5) {
            Text(agent)
            dot; Text(job.repoLabel)
            dot; Text("#\(job.issue)")
            if job.prNum > 0 { dot; Text("PR #\(job.prNum)") }
            if !age.isEmpty { dot; Text(age) }
        }
        .font(Theme.mono(11))
        .foregroundStyle(Theme.muted)
        .lineLimit(1)
    }
    private var dot: some View { Text("·").foregroundStyle(Theme.faint) }
}

// ─── glyphs + chips ───────────────────────────────────────────────────────

private struct PRGlyph: View {
    let job: Job
    let level: AttentionLevel
    var body: some View {
        if level == .working {
            ProgressView().scaleEffect(0.7).tint(Theme.emerald)
        } else if job.prNum > 0 {
            Image(systemName: "arrow.triangle.pull")
                .font(.system(size: 14, weight: .semibold))
                .foregroundStyle(Theme.orchid)
        } else {
            PulseDot(color: level.color, pulsing: level == .needsYou)
        }
    }
}

private struct PulseDot: View {
    let color: Color
    var pulsing: Bool
    @State private var on = false
    var body: some View {
        Circle().fill(color).frame(width: 9, height: 9)
            .overlay {
                if pulsing {
                    Circle().stroke(color, lineWidth: 2)
                        .scaleEffect(on ? 2.1 : 1).opacity(on ? 0 : 0.7)
                        .animation(.easeOut(duration: 1.6).repeatForever(autoreverses: false), value: on)
                }
            }
            .onAppear { if pulsing { on = true } }
    }
}

private struct StatusChip: View {
    let job: Job
    let att: Attention

    var body: some View {
        switch att.level {
        case .needsYou:
            chip("needs you", fg: Theme.rose, bg: Theme.rose.opacity(0.13))
        case .watching:
            switch ciStatus(job.lastCheckConclusions) {
            case .pass: chip("✓ CI", fg: Theme.emerald, bg: Theme.emerald.opacity(0.13))
            case .fail: chip("✕ CI", fg: Theme.rose,    bg: Theme.rose.opacity(0.13))
            default:    chip("review", fg: Theme.amber, bg: Theme.amber.opacity(0.13))
            }
        case .working:
            chip("working", fg: Theme.emerald, bg: Theme.emerald.opacity(0.12))
        case .quiet:
            EmptyView()
        }
    }

    private func chip(_ text: String, fg: Color, bg: Color) -> some View {
        Text(text)
            .font(Theme.mono(10, weight: .medium))
            .foregroundStyle(fg)
            .padding(.horizontal, 7).padding(.vertical, 3)
            .background(bg, in: Capsule())
            .fixedSize()
    }
}

// ─── header + chrome ──────────────────────────────────────────────────────

private struct SectionHeader: View {
    let title: String
    let count: Int
    let color: Color
    var body: some View {
        HStack(spacing: 7) {
            Circle().fill(color).frame(width: 6, height: 6)
            Text(title.uppercased())
                .font(Theme.mono(10, weight: .semibold))
                .tracking(1.2)
                .foregroundStyle(Theme.muted)
            Text("\(count)").font(Theme.mono(10)).foregroundStyle(Theme.faint)
            Spacer()
        }
        .textCase(nil)
        .padding(.vertical, 2)
    }
}

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
            Circle().fill(c).frame(width: 7, height: 7)
            Text(label).font(Theme.mono(10)).foregroundStyle(Theme.muted)
        }
    }
}

private struct EmptyState: View {
    let error: String?
    var body: some View {
        VStack(spacing: 10) {
            Image(systemName: error == nil ? "tray" : "wifi.exclamationmark")
                .font(.system(size: 42, weight: .thin))
                .foregroundStyle(Theme.faint)
            Text(error == nil ? "No active sessions" : "Couldn't reach orchid")
                .font(Theme.mono(13)).foregroundStyle(Theme.ink)
            Text(error ?? "File an inbox issue to spawn one.")
                .font(.caption).foregroundStyle(Theme.muted)
                .multilineTextAlignment(.center)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .padding(40)
        .background(Theme.surface)
    }
}

private struct SetupCard: View {
    var body: some View {
        VStack(spacing: 12) {
            Text("Orchid").font(Theme.wordmark(40)).foregroundStyle(Theme.ink)
            Image(systemName: "gearshape").font(.system(size: 40, weight: .thin))
                .foregroundStyle(Theme.faint)
            Text("Add your orchid endpoint")
                .font(Theme.mono(13)).foregroundStyle(Theme.ink)
            Text("Settings tab → endpoint + dashboard token.")
                .font(.caption).foregroundStyle(Theme.muted)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .padding(40)
        .background(Theme.surface)
    }
}

// attention-level → swatch
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
