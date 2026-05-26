import SwiftUI

/// Jobs list — mirrors the dashboard "list view": three groups
/// (Needs you / Working / Awaiting review) over /api/state.
struct JobsView: View {
    @AppStorage("orchid.endpoint") private var endpoint: String = ""
    @AppStorage("orchid.token")    private var captureToken: String = ""
    @AppStorage("orchid.http_secret") private var httpSecret: String = ""

    @State private var jobs: [JobRow] = []
    @State private var loading = false
    @State private var lastError: String?
    @State private var refreshTimer: Timer?

    var body: some View {
        NavigationStack {
            Group {
                if endpoint.isEmpty {
                    setupCard
                } else if jobs.isEmpty && lastError == nil {
                    ContentUnavailableView("No jobs yet",
                        systemImage: "tray",
                        description: Text("File an inbox issue to spawn a session."))
                } else if let err = lastError, jobs.isEmpty {
                    ContentUnavailableView("Couldn't reach orchid",
                        systemImage: "wifi.exclamationmark",
                        description: Text(err))
                } else {
                    list
                }
            }
            .navigationTitle("Orchid")
            .navigationBarTitleDisplayMode(.large)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button { Task { await refresh() } } label: {
                        Image(systemName: "arrow.clockwise")
                    }
                    .disabled(loading)
                }
            }
            .refreshable { await refresh() }
        }
        .task { await refresh() }
        .onAppear {
            refreshTimer = Timer.scheduledTimer(withTimeInterval: 8, repeats: true) { _ in
                Task { await refresh() }
            }
        }
        .onDisappear { refreshTimer?.invalidate(); refreshTimer = nil }
    }

    private var setupCard: some View {
        VStack(spacing: 12) {
            Image(systemName: "gearshape")
                .font(.system(size: 48, weight: .thin))
                .foregroundStyle(.secondary)
            Text("Add your orchid endpoint")
                .font(.system(.body, design: .monospaced))
            Text("Open Capture → settings to set it.")
                .font(.caption)
                .foregroundStyle(.secondary)
        }
        .padding(40)
    }

    private var list: some View {
        List {
            section("Needs you", color: .pink,    items: jobs.filter { $0.group == .needs })
            section("Working",   color: .green,   items: jobs.filter { $0.group == .working })
            section("Awaiting review", color: .orange, items: jobs.filter { $0.group == .review })
            section("Quiet",     color: .gray,    items: jobs.filter { $0.group == .quiet })
        }
        .listStyle(.plain)
    }

    @ViewBuilder
    private func section(_ title: String, color: Color, items: [JobRow]) -> some View {
        if !items.isEmpty {
            Section {
                ForEach(items) { row in
                    NavigationLink {
                        PaneView(tmux: row.id, title: "\(row.repo) · #\(row.issue)")
                    } label: {
                        JobCell(row: row, dotColor: color)
                    }
                }
            } header: {
                HStack {
                    Text(title)
                        .font(.system(.title3, design: .serif).italic())
                        .foregroundStyle(.primary)
                    Text("\(items.count)")
                        .font(.system(.caption2, design: .monospaced))
                        .foregroundStyle(.secondary)
                    Spacer()
                }
                .textCase(nil)
                .padding(.top, 4)
            }
        }
    }

    private func refresh() async {
        loading = true; defer { loading = false }
        guard let base = baseURL(),
              let stateURL = URL(string: "\(base.absoluteString)/api/state")
        else { lastError = "bad endpoint"; return }
        var req = URLRequest(url: stateURL)
        req.timeoutInterval = 8
        if !httpSecret.isEmpty {
            req.setValue("Bearer \(httpSecret)", forHTTPHeaderField: "Authorization")
        }
        do {
            let (data, resp) = try await URLSession.shared.data(for: req)
            guard let http = resp as? HTTPURLResponse else { lastError = "no http"; return }
            if http.statusCode == 401 || http.statusCode == 403 {
                lastError = "auth rejected — add http_secret in Capture settings"
                return
            }
            if http.statusCode >= 400 {
                lastError = "http \(http.statusCode)"; return
            }
            let decoded = try JSONDecoder().decode(StateResp.self, from: data)
            jobs = decoded.jobs.map(JobRow.from)
            lastError = nil
        } catch {
            lastError = error.localizedDescription
        }
    }

    private func baseURL() -> URL? {
        guard let u = URL(string: endpoint) else { return nil }
        var s = u.absoluteString
        for suffix in ["/api/drafts", "/api/state", "/"] {
            if s.hasSuffix(suffix) { s.removeLast(suffix.count); break }
        }
        return URL(string: s)
    }
}

private struct JobCell: View {
    let row: JobRow
    let dotColor: Color

    var body: some View {
        HStack(alignment: .top, spacing: 12) {
            Circle().fill(dotColor).frame(width: 8, height: 8)
                .padding(.top, 7)
            VStack(alignment: .leading, spacing: 3) {
                Text(row.title.isEmpty ? "—" : row.title)
                    .font(.system(.body))
                    .foregroundStyle(.primary)
                HStack(spacing: 4) {
                    Text(row.repo)
                        .font(.system(.caption2, design: .monospaced))
                        .foregroundStyle(.secondary)
                    Text("·").foregroundStyle(.secondary)
                    Text("#\(row.issue)")
                        .font(.system(.caption2, design: .monospaced))
                        .foregroundStyle(.secondary)
                    if row.pr > 0 {
                        Text("·").foregroundStyle(.secondary)
                        Text("PR #\(row.pr)")
                            .font(.system(.caption2, design: .monospaced))
                            .foregroundStyle(.secondary)
                    }
                }
            }
            Spacer()
        }
        .padding(.vertical, 4)
    }
}

// ─── data ───────────────────────────────────────────────────────────

private struct StateResp: Decodable {
    let jobs: [JobAPI]
}

private struct JobAPI: Decodable {
    let issue: Int
    let tmux: String
    let target: String?
    let target_repo: String?
    let issue_title: String?
    let pr: Int?
    let needs_input: Bool?
    let last_check_conclusions: [String: String]?
}

struct JobRow: Identifiable {
    let id: String
    let issue: Int
    let pr: Int
    let title: String
    let repo: String
    let group: Group
    enum Group { case needs, working, review, quiet }

    static func from(_ j: JobAPI) -> JobRow {
        let repoLabel: String = {
            if let r = j.target_repo, !r.isEmpty { return String(r.split(separator: "/").last ?? Substring(r)) }
            return j.target ?? "—"
        }()
        let group: Group = {
            if j.needs_input == true { return .needs }
            if j.pr ?? 0 > 0 {
                if let checks = j.last_check_conclusions {
                    if checks.values.contains("FAILURE") { return .needs }
                    if checks.values.contains("IN_PROGRESS") || checks.values.contains("PENDING") { return .working }
                }
                return .review
            }
            return .working
        }()
        return JobRow(
            id: j.tmux,
            issue: j.issue,
            pr: j.pr ?? 0,
            title: j.issue_title ?? "",
            repo: repoLabel,
            group: group
        )
    }
}
