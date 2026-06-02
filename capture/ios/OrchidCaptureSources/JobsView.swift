import SwiftUI

/// Sessions — a 1:1 port of the web dashboard ListView: one flat, hairline-
/// divided list. Live sessions sort by attention (needs-you first), closed
/// "ghosts" are deduped one-per-issue and sink to the bottom, dimmed. Each
/// row is the dashboard SessionRow — PR-status octicon, title, and a
/// `agent · repo · age` meta line with the needs-input tag.
struct SessionsList: View {
    @EnvironmentObject private var store: StateStore
    @AppStorage("orchid.endpoint") private var endpoint: String = ""
    let q: String

    private var rows: [Job] {
        let all = store.state.jobs ?? []
        var live: [Job] = []
        var byIssue: [Int: Job] = [:]
        for j in all {
            if j.isClosed {
                if let p = byIssue[j.issue], (p.closedAt ?? 0) >= (j.closedAt ?? 0) { continue }
                byIssue[j.issue] = j
            } else if !j.tmuxName.isEmpty {
                live.append(j)
            }
        }
        var merged = live + Array(byIssue.values)
        let term = q.trimmingCharacters(in: .whitespaces).lowercased()
        if !term.isEmpty {
            merged = merged.filter {
                ($0.issueTitle ?? "").lowercased().contains(term)
                || ($0.targetRepo ?? "").lowercased().contains(term)
                || "\($0.issue)".contains(term)
                || $0.tmuxName.lowercased().contains(term)
            }
        }
        func rank(_ j: Job) -> Int {
            if j.isClosed { return 4 }
            switch attention(j).level {
            case .needsYou: return 0; case .working: return 1
            case .watching: return 2; case .quiet: return 3
            }
        }
        func ts(_ j: Job) -> Double { isoMillis(j.spawnedAt) }
        return merged.sorted { a, b in
            let (ra, rb) = (rank(a), rank(b))
            return ra != rb ? ra < rb : ts(a) > ts(b)
        }
    }

    var body: some View {
        Group {
            if endpoint.isEmpty {
                Hint(icon: "gearshape", title: "Add your endpoint",
                     detail: "Settings → endpoint + dashboard token.")
            } else if rows.isEmpty {
                empty
            } else {
                ScrollView {
                    LazyVStack(spacing: 0) {
                        ForEach(rows) { job in
                            row(job)
                            Rectangle().fill(Theme.line).frame(height: 1).padding(.leading, 12)
                        }
                    }
                }
                .refreshable { store.reconnect() }
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Theme.surface)
    }

    @ViewBuilder private func row(_ job: Job) -> some View {
        if job.isClosed || job.tmuxName.isEmpty {
            SessionRow(job: job)
        } else {
            NavigationLink {
                PaneView(tmux: job.tmuxName, title: "\(job.repoLabel) · #\(job.issue)")
            } label: { SessionRow(job: job) }
            .buttonStyle(.plain)
        }
    }

    private var empty: some View {
        VStack(spacing: 6) {
            Text(q.isEmpty ? "Empty" : "No matches")
                .font(Theme.serif(28).italic()).foregroundStyle(Theme.muted)
            Text(q.isEmpty ? "Open an inbox issue to spawn a session." : "Try a different search.")
                .font(.system(size: 13)).foregroundStyle(Theme.faint)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

// ─── row ──────────────────────────────────────────────────────────────────

struct SessionRow: View {
    let job: Job
    private var ghost: Bool { job.isClosed }
    private var att: Attention { attention(job) }
    private var needs: Bool { !ghost && att.level == .needsYou }

    var body: some View {
        HStack(spacing: 12) {
            PrStatusIcon(job: job, active: att.level == .working)
            VStack(alignment: .leading, spacing: 2) {
                Text(job.issueTitle?.isEmpty == false ? job.issueTitle! : (job.tmuxName.isEmpty ? "—" : job.tmuxName))
                    .font(.system(size: 14, weight: ghost ? .regular : (needs ? .semibold : .medium)))
                    .foregroundStyle(ghost ? Theme.muted : Theme.ink)
                    .lineLimit(1)
                meta
            }
            Spacer(minLength: 4)
        }
        .padding(.vertical, 10).padding(.horizontal, 12)
        .overlay(alignment: .leading) {
            if needs { Rectangle().fill(Theme.rose).frame(width: 2) }
        }
        .background(needs ? Theme.rose.opacity(0.06) : Color.clear)
        .opacity(ghost ? 0.55 : 1)
        .contentShape(Rectangle())
    }

    private var agent: String {
        let t = job.tmuxName.lowercased()
        if t.hasPrefix("codex") { return "codex" }
        if t.hasPrefix("claude") { return "claude" }
        return "unknown"
    }

    private var meta: some View {
        HStack(spacing: 6) {
            AgentMark(agent: agent)
            Text(job.repoLabel).font(Theme.mono(11)).foregroundStyle(Theme.muted)
            if ghost {
                Text(job.closedState ?? "")
                    .font(Theme.mono(11))
                    .foregroundStyle(job.closedState == "merged" ? Theme.violet : Theme.muted)
            } else {
                let age = relAge(job.spawnedAt)
                if !age.isEmpty {
                    Text("·").foregroundStyle(Theme.faint)
                    Text(age).font(Theme.mono(11)).foregroundStyle(Theme.muted)
                }
                if needs {
                    Text((job.needsInput ?? false) ? "needs input" : att.reason)
                        .font(Theme.mono(9, weight: .medium)).tracking(0.5)
                        .foregroundStyle(Theme.rose)
                        .padding(.horizontal, 6).padding(.vertical, 1)
                        .background(Theme.rose.opacity(0.14), in: Capsule())
                }
            }
        }
        .lineLimit(1)
    }
}

/// PR-status glyph — exact mapping from the dashboard PrStatusIcon.
struct PrStatusIcon: View {
    let job: Job
    let active: Bool
    @State private var pulse = false

    var body: some View {
        Group {
            if job.closedState == "merged" {
                mark("octicon-merge", Theme.violet)
            } else if job.closedState == "closed" {
                Image(systemName: "xmark.circle").font(.system(size: 15, weight: .semibold)).foregroundStyle(Theme.faint)
            } else if job.prNum > 0, ciStatus(job.lastCheckConclusions) == .fail {
                Image(systemName: "xmark.circle").font(.system(size: 15, weight: .semibold)).foregroundStyle(Theme.rose)
            } else if job.prNum > 0, ciStatus(job.lastCheckConclusions) == .pass {
                Image(systemName: "checkmark.circle").font(.system(size: 15, weight: .semibold)).foregroundStyle(Theme.emerald)
            } else if job.prNum > 0 {
                mark("octicon-pr", Theme.emerald)
            } else {
                Circle().fill(Theme.sky).frame(width: 10, height: 10)
                    .opacity(active && pulse ? 0.4 : 1)
                    .animation(active ? .easeInOut(duration: 0.9).repeatForever() : .default, value: pulse)
                    .onAppear { if active { pulse = true } }
            }
        }
        .frame(width: 20, height: 20)
    }

    private func mark(_ name: String, _ color: Color) -> some View {
        Image(name).renderingMode(.template).resizable().scaledToFit()
            .frame(width: 15, height: 15).foregroundStyle(color)
    }
}

/// Provider brand mark (Claude / OpenAI), bundled SVGs tinted to match the
/// web AgentMark.
struct AgentMark: View {
    let agent: String
    var body: some View {
        switch agent {
        case "claude":
            Image("claude-mark").renderingMode(.template).resizable().scaledToFit()
                .frame(width: 13, height: 13).foregroundStyle(Theme.claudeMark)
        case "codex":
            Image("openai-mark").renderingMode(.template).resizable().scaledToFit()
                .frame(width: 13, height: 13).foregroundStyle(Theme.ink)
        default:
            Circle().fill(Theme.zinc).frame(width: 11, height: 11)
        }
    }
}

// ─── shared ────────────────────────────────────────────────────────────────

struct Hint: View {
    let icon: String
    let title: String
    let detail: String
    var body: some View {
        VStack(spacing: 9) {
            Image(systemName: icon).font(.system(size: 34, weight: .light)).foregroundStyle(Theme.faint)
            Text(title).font(.system(size: 15, weight: .medium)).foregroundStyle(Theme.ink)
            Text(detail).font(.footnote).foregroundStyle(Theme.muted).multilineTextAlignment(.center)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity).padding(40)
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

/// RFC3339 → epoch millis (0 if unknown), for sort.
func isoMillis(_ s: String?) -> Double {
    guard let s = s, !s.isEmpty, !s.hasPrefix("0001") else { return 0 }
    let f = ISO8601DateFormatter(); f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    if let d = f.date(from: s) { return d.timeIntervalSince1970 * 1000 }
    let f2 = ISO8601DateFormatter()
    return (f2.date(from: s)?.timeIntervalSince1970 ?? 0) * 1000
}
