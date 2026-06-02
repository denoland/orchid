import SwiftUI

/// Machines — the telemetry sidebar from the web dashboard, as its own
/// tab: per-account usage & pacing (5h + 7d quota bars with the linear
/// pace marker + governor burn) and the VM fleet list with online dots.
struct MachinesView: View {
    @EnvironmentObject private var store: StateStore

    private var agents: [(String, AgentMeter)] {
        if let a = store.state.agents, !a.isEmpty {
            return a.keys.sorted().map { ($0, a[$0]!) }
        }
        // Back-compat: top-level quota/governor mirror "claude".
        if store.state.quota != nil || store.state.governor != nil {
            return [("claude", AgentMeter(quota: store.state.quota, governor: store.state.governor))]
        }
        return []
    }

    private var vms: [VM] { store.state.vms ?? [] }

    var body: some View {
        NavigationStack {
            List {
                if !agents.isEmpty {
                    Section {
                        ForEach(agents, id: \.0) { name, meter in
                            UsageStrip(account: name, meter: meter)
                                .listRowBackground(Theme.panel)
                        }
                    } header: { header("Usage & pacing") }
                }
                if !vms.isEmpty {
                    Section {
                        ForEach(vms) { vm in
                            VMRow(vm: vm).listRowBackground(Theme.panel)
                        }
                    } header: { header("Machines") }
                }
                if agents.isEmpty && vms.isEmpty {
                    Text(store.lastError ?? "No telemetry yet")
                        .font(Theme.mono(12)).foregroundStyle(Theme.muted)
                        .listRowBackground(Theme.surface)
                }
            }
            .listStyle(.insetGrouped)
            .scrollContentBackground(.hidden)
            .background(Theme.surface)
            .navigationTitle("")
            .toolbar {
                ToolbarItem(placement: .topBarLeading) {
                    Text("Machines").font(Theme.wordmark(28)).foregroundStyle(Theme.ink)
                }
                ToolbarItem(placement: .topBarTrailing) { ConnDot(link: store.link) }
            }
            .toolbarBackground(Theme.surface, for: .navigationBar)
        }
        .tint(Theme.orchid)
        .onAppear { store.start() }
    }

    private func header(_ s: String) -> some View {
        Text(s.uppercased())
            .font(Theme.mono(10, weight: .semibold)).tracking(1.2)
            .foregroundStyle(Theme.muted).textCase(nil)
    }
}

// ─── per-account usage strip ──────────────────────────────────────────────

struct UsageStrip: View {
    let account: String
    let meter: AgentMeter

    private var q: Quota? { meter.quota }
    private var g: Governor? { meter.governor }
    private var throttled: Bool { (q?.throttle?.mode ?? "allow") != "allow" }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 8) {
                AgentMark(account: account)
                Text(account).font(.system(size: 14, weight: .medium)).foregroundStyle(Theme.ink)
                if let plan = q?.planType, !plan.isEmpty {
                    Text(plan).font(Theme.mono(9)).foregroundStyle(Theme.faint)
                        .padding(.horizontal, 5).padding(.vertical, 1)
                        .background(Theme.line, in: Capsule())
                }
                Spacer()
                if throttled {
                    Text("throttled").font(Theme.mono(10, weight: .medium))
                        .foregroundStyle(Theme.amber)
                }
            }

            // 7-day (weekly) — the bucket with the pace marker.
            QuotaBar(pct: q?.sevenDayPct ?? 0,
                     label: "7d",
                     marker: q?.throttle?.targetPct,
                     over: throttled)
            // 5-hour — thinner.
            QuotaBar(pct: q?.fiveHourPct ?? 0, label: "5h", marker: nil, over: false, thin: true)

            if let g, (g.active ?? 0) > 0 || (g.effectiveCap ?? -1) >= 0 {
                HStack(spacing: 10) {
                    label("active", "\(g.active ?? 0)")
                    if let cap = g.effectiveCap, cap >= 0 { label("cap", "\(cap)") }
                    if let p = g.paused, p > 0 { label("paused", "\(p)") }
                    if let b = g.binding, !b.isEmpty { label("binding", b) }
                    Spacer()
                }
                .font(Theme.mono(10))
            }
            if let reason = q?.throttle?.reason, !reason.isEmpty, throttled {
                Text(reason).font(Theme.mono(10)).foregroundStyle(Theme.muted).lineLimit(2)
            }
        }
        .padding(.vertical, 4)
    }

    private func label(_ k: String, _ v: String) -> some View {
        HStack(spacing: 3) {
            Text(k).foregroundStyle(Theme.faint)
            Text(v).foregroundStyle(Theme.muted)
        }
    }
}

struct QuotaBar: View {
    let pct: Double
    let label: String
    var marker: Double?
    var over: Bool
    var thin: Bool = false

    private var fill: Color {
        if over { return Theme.amber }
        return pct >= 90 ? Theme.rose : Theme.emerald
    }

    var body: some View {
        HStack(spacing: 8) {
            Text(label).font(Theme.mono(9)).foregroundStyle(Theme.faint).frame(width: 16, alignment: .leading)
            GeometryReader { geo in
                ZStack(alignment: .leading) {
                    Capsule().fill(Theme.line)
                    Capsule().fill(fill)
                        .frame(width: max(0, min(1, pct / 100)) * geo.size.width)
                    if let m = marker, m > 0, m < 100 {
                        Rectangle().fill(Theme.ink.opacity(0.55))
                            .frame(width: 1.5)
                            .offset(x: (m / 100) * geo.size.width)
                    }
                }
            }
            .frame(height: thin ? 4 : 7)
            Text("\(Int(pct.rounded()))%").font(Theme.mono(9))
                .foregroundStyle(Theme.muted).frame(width: 30, alignment: .trailing)
        }
    }
}

// ─── VM row ───────────────────────────────────────────────────────────────

struct VMRow: View {
    let vm: VM
    var body: some View {
        HStack(spacing: 11) {
            Circle().fill((vm.online ?? false) ? Theme.emerald : Theme.faint)
                .frame(width: 8, height: 8)
            VStack(alignment: .leading, spacing: 2) {
                Text(vm.name).font(.system(size: 14)).foregroundStyle(Theme.ink)
                HStack(spacing: 5) {
                    if let a = vm.agent { Text(a) }
                    if let os = vm.os { Text("·").foregroundStyle(Theme.faint); Text(os) }
                    if let h = vm.host { Text("·").foregroundStyle(Theme.faint); Text(h) }
                }
                .font(Theme.mono(10)).foregroundStyle(Theme.muted).lineLimit(1)
            }
            Spacer()
            Text("\(vm.used ?? 0)/\(vm.capacity ?? 0)")
                .font(Theme.mono(12, weight: .medium))
                .foregroundStyle((vm.used ?? 0) >= (vm.capacity ?? 0) ? Theme.amber : Theme.muted)
        }
        .padding(.vertical, 3)
    }
}

/// Provider monogram for an account (claude / codex). Small, native — the
/// row meta already names the agent, so this is just a glyph.
struct AgentMark: View {
    let account: String
    var body: some View {
        let codex = account.hasPrefix("codex")
        Text(codex ? "O" : "A")
            .font(.system(size: 11, weight: .bold, design: .rounded))
            .foregroundStyle(Theme.panel)
            .frame(width: 18, height: 18)
            .background(codex ? Theme.ink : Theme.orchid, in: RoundedRectangle(cornerRadius: 5))
    }
}
