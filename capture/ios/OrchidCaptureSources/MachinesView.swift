import SwiftUI

/// Machines — the dashboard telemetry sidebar as a tab body: per-account
/// usage & pacing (7d + 5h bars with the pace marker) and the VM fleet.
struct MachinesContent: View {
    @EnvironmentObject private var store: StateStore

    private var agents: [(String, AgentMeter)] {
        if let a = store.state.agents, !a.isEmpty { return a.keys.sorted().map { ($0, a[$0]!) } }
        if store.state.quota != nil || store.state.governor != nil {
            return [("claude", AgentMeter(quota: store.state.quota, governor: store.state.governor))]
        }
        return []
    }
    private var vms: [VM] { store.state.vms ?? [] }

    var body: some View {
        Group {
            if agents.isEmpty && vms.isEmpty {
                Hint(icon: "server.rack", title: "No telemetry", detail: store.lastError ?? "Waiting for orchid state.")
            } else {
                ScrollView {
                    VStack(alignment: .leading, spacing: 0) {
                        if !agents.isEmpty {
                            header("Usage & pacing")
                            ForEach(agents, id: \.0) { UsageStrip(account: $0.0, meter: $0.1) }
                        }
                        if !vms.isEmpty {
                            header("Machines").padding(.top, 18)
                            ForEach(vms) { VMRow(vm: $0) }
                        }
                    }
                    .padding(.vertical, 12)
                }
                .refreshable { store.reconnect() }
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Theme.surface)
    }

    private func header(_ s: String) -> some View {
        Text(s.uppercased()).font(Theme.mono(9, weight: .semibold)).tracking(1.8)
            .foregroundStyle(Theme.faint)
            .padding(.horizontal, 16).padding(.bottom, 8)
    }
}

enum Hot { case none, amber, red }

/// 1:1 with the web QuotaStrip (stacked variant): left column = brand mark +
/// label + the 5h/7d bars + governor line; right column = the big weekly %.
struct UsageStrip: View {
    let account: String
    let meter: AgentMeter
    private var q: Quota? { meter.quota }
    private var g: Governor? { meter.governor }

    private var sevenHot: Hot {
        switch q?.throttle?.mode {
        case "pause_5h", "pause_week": return .red
        case "throttle": return .amber
        default: return .none
        }
    }
    private var bigColor: Color {
        switch sevenHot { case .red: return Theme.rose; case .amber: return Theme.amber; case .none: return Theme.ink }
    }

    var body: some View {
        HStack(spacing: 12) {
            VStack(alignment: .leading, spacing: 8) {
                HStack(spacing: 6) {
                    AgentMark(agent: account.hasPrefix("codex") ? "codex" : "claude")
                    Text(account).font(Theme.mono(12)).foregroundStyle(Theme.muted)
                    if let plan = q?.planType, !plan.isEmpty {
                        Text(plan).font(Theme.mono(9)).foregroundStyle(Theme.faint)
                            .padding(.horizontal, 4).padding(.vertical, 1)
                            .background(Theme.searchBg, in: RoundedRectangle(cornerRadius: 4))
                    }
                }
                VStack(alignment: .leading, spacing: 6) {
                    QuotaBar(label: "5h", pct: q?.fiveHourPct ?? 0,
                             resets: q?.fiveHourResetsAt ?? 0, window: 5 * 3600)
                    QuotaBar(label: "7d", pct: q?.sevenDayPct ?? 0,
                             resets: q?.sevenDayResetsAt ?? 0, window: 7 * 24 * 3600,
                             forcedHot: sevenHot, target: q?.throttle?.targetPct)
                }
                if let g, g.enabled == true { GovStrip(g: g) }
            }
            Spacer(minLength: 8)
            VStack(spacing: 3) {
                HStack(alignment: .firstTextBaseline, spacing: 1) {
                    Text("\(Int((q?.sevenDayPct ?? 0).rounded()))")
                        .font(Theme.mono(32, weight: .semibold)).foregroundStyle(bigColor)
                    Text("%").font(.system(size: 14)).foregroundStyle(Theme.muted)
                }
                Text("7D").font(Theme.mono(9)).tracking(2).foregroundStyle(Theme.faint)
            }
        }
        .padding(12)
        .background(Theme.panel, in: RoundedRectangle(cornerRadius: 10))
        .overlay(RoundedRectangle(cornerRadius: 10).stroke(Theme.line, lineWidth: 1))
        .padding(.horizontal, 16).padding(.vertical, 5)
    }
}

struct QuotaBar: View {
    let label: String
    let pct: Double
    let resets: Double
    let window: Double
    var forcedHot: Hot? = nil
    var target: Double? = nil

    private var hot: Hot {
        if let f = forcedHot { return f }
        let elapsed = min(100, max(0, (1 - max(0, resets - now()) / window) * 100))
        return pct > elapsed + 5 ? .amber : .none
    }
    private var track: Color {
        switch hot { case .red: return Theme.rose.opacity(0.28); case .amber: return Theme.amber.opacity(0.28); case .none: return Theme.line }
    }
    private var fill: Color {
        switch hot { case .red: return Theme.rose; case .amber: return Theme.amber; case .none: return Theme.emerald }
    }

    var body: some View {
        HStack(spacing: 6) {
            Text(label).font(Theme.mono(10)).foregroundStyle(Theme.muted).frame(width: 18, alignment: .leading)
            ZStack(alignment: .leading) {
                Capsule().fill(track)
                Capsule().fill(fill).frame(width: clamp(pct) / 100 * 80)
                if let t = target, t > 0, t < 100 {
                    Rectangle().fill(Theme.muted.opacity(0.7)).frame(width: 1)
                        .offset(x: clamp(t) / 100 * 80)
                }
            }
            .frame(width: 80, height: 6)
            Text("\(Int(pct.rounded()))%").font(Theme.mono(10)).foregroundStyle(Theme.muted)
                .frame(width: 30, alignment: .leading)
            Text(fmtReset(resets - now())).font(Theme.mono(10)).foregroundStyle(Theme.faint)
        }
    }
    private func clamp(_ v: Double) -> Double { min(100, max(0, v)) }
}

/// Compact governor readout — cap (active/paused) · burn≷target · →proj%.
struct GovStrip: View {
    let g: Governor
    var body: some View {
        let onFive = g.binding == "5h"
        let burn = onFive ? (g.burnFive ?? 0) : (g.burnWeekly ?? 0)
        let target = onFive ? (g.targetFive ?? 0) : (g.targetWeekly ?? 0)
        let over = burn > target + 0.05
        let cap = (g.effectiveCap ?? -1) < 0 ? "∞" : "\(g.effectiveCap ?? 0)"
        let proj = g.projectedEndPct ?? 0
        return HStack(spacing: 6) {
            Text("cap \(cap) (\(g.active ?? 0)\((g.paused ?? 0) > 0 ? "/\(g.paused ?? 0)❄" : ""))")
                .foregroundStyle(Theme.muted)
            Text("\(burn, specifier: "%.1f")\(over ? "›" : "≤")\(target, specifier: "%.1f")%/h")
                .foregroundStyle(over ? Theme.amber : Theme.muted)
            Text("→\(Int(proj))%").foregroundStyle(proj > 92 ? Theme.amber : Theme.muted)
        }
        .font(Theme.mono(10))
    }
}

private func now() -> Double { Date().timeIntervalSince1970 }
private func fmtReset(_ secs: Double) -> String {
    if secs <= 0 { return "now" }
    let h = Int(secs) / 3600, m = (Int(secs) % 3600) / 60, d = Int(secs) / 86400
    if d > 0 { return "\(d)d\(h % 24)h" }
    if h > 0 { return "\(h)h\(m)m" }
    return "\(m)m"
}

struct VMRow: View {
    let vm: VM
    private var full: Bool { (vm.used ?? 0) >= (vm.capacity ?? 0) }
    var body: some View {
        HStack(spacing: 8) {
            Circle().fill((vm.online ?? false) ? Theme.emerald : Theme.zinc).frame(width: 6, height: 6)
            Text(vm.name).font(Theme.mono(12)).foregroundStyle(Theme.ink)
            if let a = vm.agent { Text(a).font(Theme.mono(10)).foregroundStyle(Theme.faint) }
            Spacer()
            Text("\(vm.used ?? 0)/\(vm.capacity ?? 0)")
                .font(Theme.mono(11)).foregroundStyle(full ? Theme.amber : Theme.muted)
        }
        .padding(.horizontal, 16).padding(.vertical, 7)
    }
}
