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

struct UsageStrip: View {
    let account: String
    let meter: AgentMeter
    private var q: Quota? { meter.quota }
    private var throttled: Bool { (q?.throttle?.mode ?? "allow") != "allow" }
    private var tint: Color { account.hasPrefix("codex") ? Theme.emerald : Theme.violet }

    var body: some View {
        VStack(alignment: .leading, spacing: 7) {
            HStack(spacing: 7) {
                AgentMark(agent: account.hasPrefix("codex") ? "codex" : "claude")
                Text(account).font(.system(size: 14, weight: .medium)).foregroundStyle(Theme.ink)
                if let plan = q?.planType, !plan.isEmpty {
                    Text(plan).font(Theme.mono(9)).foregroundStyle(Theme.faint)
                }
                Spacer()
                Text("\(Int((q?.sevenDayPct ?? 0).rounded()))%")
                    .font(Theme.mono(13)).foregroundStyle(throttled ? Theme.amber : Theme.muted)
            }
            QuotaBar(pct: q?.sevenDayPct ?? 0, label: "7d", marker: q?.throttle?.targetPct,
                     fill: throttled ? Theme.amber : tint)
            QuotaBar(pct: q?.fiveHourPct ?? 0, label: "5h", marker: nil, fill: tint, thin: true)
            if throttled, let reason = q?.throttle?.reason, !reason.isEmpty {
                Text(reason).font(Theme.mono(10)).foregroundStyle(Theme.muted).lineLimit(2)
            }
        }
        .padding(.horizontal, 16).padding(.vertical, 9)
    }
}

struct QuotaBar: View {
    let pct: Double
    let label: String
    var marker: Double?
    var fill: Color
    var thin: Bool = false
    var body: some View {
        HStack(spacing: 8) {
            Text(label).font(Theme.mono(9)).foregroundStyle(Theme.faint).frame(width: 16, alignment: .leading)
            GeometryReader { geo in
                ZStack(alignment: .leading) {
                    Capsule().fill(Theme.line)
                    Capsule().fill(pct >= 95 ? Theme.rose : fill)
                        .frame(width: max(0, min(1, pct / 100)) * geo.size.width)
                    if let m = marker, m > 0, m < 100 {
                        Rectangle().fill(Theme.ink.opacity(0.5)).frame(width: 1.5)
                            .offset(x: (m / 100) * geo.size.width)
                    }
                }
            }
            .frame(height: thin ? 3 : 5)
        }
    }
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
