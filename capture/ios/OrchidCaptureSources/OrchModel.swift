import Foundation

// Codable mirror of www/src/types.ts. Decoded with
// `.convertFromSnakeCase`, so properties stay camelCase. Fields the Go
// side emits with `omitempty` are optional here.

struct PaneUsage: Codable, Hashable {
    var model: String?
    var costUsd: Double?
    var contextPct: Double?
}

struct WIP: Codable, Hashable {
    var files: Int?
    var added: Int?
    var removed: Int?
    var ahead: Int?
    var ok: Bool?
}

struct Job: Codable, Hashable, Identifiable {
    var issue: Int
    var vm: String?
    var tmux: String?
    var target: String?
    var targetRepo: String?
    var branch: String?
    var issueTitle: String?
    var lifecycle: String?
    var schedule: String?
    var pr: Int?
    var nextFireAt: String?
    var lastCheckConclusions: [String: String]?
    var activity: [Int]?
    var currentAction: String?
    var spawnedAt: String?
    var wip: WIP?
    var needsInput: Bool?
    var vmOnline: Bool?
    var usage: PaneUsage?
    var closedState: String?
    var closedAt: Double?
    var priority: Int?
    var paused: Bool?
    var pausedState: Bool?
    var pausedAt: String?

    // Stable identity: issue # disambiguates cron rows with empty tmux.
    var id: String { "\(issue)-\(tmux ?? "")" }

    var prNum: Int { pr ?? 0 }
    var tmuxName: String { tmux ?? "" }
    var isClosed: Bool { (closedState ?? "").isEmpty == false }
    var repoLabel: String {
        if let r = targetRepo, !r.isEmpty { return String(r.split(separator: "/").last ?? Substring(r)) }
        return target ?? "—"
    }
}

struct VM: Codable, Hashable, Identifiable {
    var name: String
    var host: String?
    var capacity: Int?
    var used: Int?
    var bot: String?
    var agent: String?
    var online: Bool?
    var lastErr: String?
    var os: String?

    var id: String { name }
}

struct Throttle: Codable, Hashable {
    var mode: String?            // allow | throttle | pause_5h | pause_week
    var reason: String?
    var until: Double?
    var targetPct: Double?
    var projectedExhaustAt: Double?
}

struct Quota: Codable, Hashable {
    var fiveHourPct: Double?
    var fiveHourResetsAt: Double?
    var sevenDayPct: Double?
    var sevenDayResetsAt: Double?
    var planType: String?
    var credits: Double?
    var throttle: Throttle?
}

struct Governor: Codable, Hashable {
    var enabled: Bool?
    var effectiveCap: Int?       // -1 == uncapped
    var active: Int?
    var paused: Int?
    var burnWeekly: Double?
    var targetWeekly: Double?
    var burnFive: Double?
    var targetFive: Double?
    var projectedEndPct: Double?
    var binding: String?         // weekly | 5h | ""
}

struct AgentMeter: Codable, Hashable {
    var quota: Quota?
    var governor: Governor?
}

struct OrchState: Codable {
    var jobs: [Job]? = nil
    var vms: [VM]? = nil
    var inbox: String? = nil
    var quota: Quota? = nil
    var governor: Governor? = nil
    var agents: [String: AgentMeter]? = nil

    static let empty = OrchState()
}

// ─── attention classifier (port of www/src/attention.ts) ───────────────

enum AttentionLevel: String {
    case needsYou = "needs-you"
    case watching
    case working
    case quiet

    /// Section header copy on the list.
    var title: String {
        switch self {
        case .needsYou:  return "Needs you"
        case .watching:  return "Awaiting review"
        case .working:   return "Working"
        case .quiet:     return "Quiet"
        }
    }
    var order: Int {
        switch self {
        case .needsYou: return 0
        case .working:  return 1
        case .watching: return 2
        case .quiet:    return 3
        }
    }
}

enum CIStatus { case fail, pass, pending, none }

func ciStatus(_ conclusions: [String: String]?) -> CIStatus {
    let vals = Array((conclusions ?? [:]).values)
    if vals.isEmpty { return .none }
    if vals.contains(where: { $0.range(of: "fail", options: .caseInsensitive) != nil }) { return .fail }
    if vals.allSatisfy({ $0.range(of: "success|completed", options: [.caseInsensitive, .regularExpression]) != nil }) { return .pass }
    return .pending
}

struct Attention {
    var level: AttentionLevel
    var reason: String
    var score: Int
}

func attention(_ job: Job) -> Attention {
    // Mirror attention.ts: ciStatus returns 'pending' when there are no
    // checks; the heuristics treat that as "pending".
    let raw = ciStatus(job.lastCheckConclusions)
    let ci: CIStatus = (raw == .none) ? .pending : raw
    let activity = job.activity ?? []
    let recent = activity.suffix(10).reduce(0, +)
    let overall = activity.reduce(0, +)
    let hasPR = job.prNum > 0
    let hasTmux = !job.tmuxName.isEmpty

    if hasTmux && (job.needsInput ?? false) && !(hasPR && ci != .fail) {
        return Attention(level: .needsYou, reason: "awaiting your answer", score: 110)
    }
    if hasPR && ci == .fail {
        return Attention(level: .needsYou, reason: "CI failing", score: 100)
    }
    if hasTmux && overall > 0 && recent == 0 && !hasPR {
        return Attention(level: .needsYou, reason: "idle — likely awaiting prompt", score: 90)
    }
    if hasPR && ci == .pass && recent == 0 {
        return Attention(level: .watching, reason: "PR ready — review needed", score: 70)
    }
    if hasPR && ci == .pending {
        return Attention(level: .watching, reason: "PR open — CI pending", score: 50)
    }
    if hasTmux && recent > 0 {
        return Attention(level: .working, reason: "active", score: 30)
    }
    if hasTmux {
        return Attention(level: .quiet, reason: "no recent activity", score: 20)
    }
    return Attention(level: .quiet, reason: "", score: 0)
}

// ─── small derived helpers for the UI ──────────────────────────────────

/// "4m", "2h", "3d" since an RFC3339 timestamp; "" if unknown.
func relAge(_ rfc3339: String?) -> String {
    guard let s = rfc3339, !s.isEmpty, !s.hasPrefix("0001") else { return "" }
    let fmt = ISO8601DateFormatter()
    fmt.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    let date = fmt.date(from: s) ?? ISO8601DateFormatter().date(from: s)
    guard let d = date else { return "" }
    let secs = max(0, Date().timeIntervalSince(d))
    if secs < 90          { return "\(Int(secs))s" }
    if secs < 5400        { return "\(Int(secs / 60))m" }
    if secs < 36 * 3600   { return "\(Int(secs / 3600))h" }
    return "\(Int(secs / 86400))d"
}
