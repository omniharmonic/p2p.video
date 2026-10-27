import AppKit
import StudioLinkCore
import SwiftUI

/// Confirms a guest-access change in plain words. The exact Tailscale
/// commands are under "Show details" and run only on the explicit click.
/// When Tailscale asks the owner to approve something, the sheet turns into
/// that step (Open Tailscale / Try Again) instead of waiting [SL-UX-2].
struct ConfirmPlanSheet: View {
    let plan: TailscaleSetup.Plan
    @ObservedObject var model: AppModel
    let close: () -> Void
    @State private var running = false
    @State private var outcome: AppModel.ApplyOutcome?

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            HStack(alignment: .top, spacing: 14) {
                Image(systemName: symbol)
                    .font(.system(size: 26, weight: .regular))
                    .foregroundStyle(Theme.accent)
                    .frame(width: 34)
                VStack(alignment: .leading, spacing: 6) {
                    Text(plan.title).font(.title3.weight(.semibold))
                    Text(plan.explanation).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                }
            }
            ForEach(plan.warnings, id: \.self) { w in
                Label(w, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.orange).font(.callout)
                    .fixedSize(horizontal: false, vertical: true)
            }
            switch outcome {
            case .needsApproval(let approval)?:
                ApprovalCard(approval: approval, busy: running) { start() }
            case .failed(let message)?:
                Label(message, systemImage: "xmark.octagon.fill").foregroundStyle(.red).font(.callout)
                    .fixedSize(horizontal: false, vertical: true).textSelection(.enabled)
            case .done?:
                Label("Done. \(doneText)", systemImage: "checkmark.circle.fill").foregroundStyle(.green).font(.callout.weight(.medium))
            case nil:
                EmptyView()
            }
            DetailsDisclosure {
                VStack(alignment: .leading, spacing: 8) {
                    Text(plan.details).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    Text("p2p.video runs exactly:").font(.caption.weight(.medium))
                    CommandList(commands: plan.commands.map(\.display))
                    if !model.tailscale.transcript.isEmpty, outcome != nil {
                        Text("Tailscale’s output").font(.caption.weight(.medium))
                        ScrollView {
                            Text(model.tailscale.transcript.joined(separator: "\n"))
                                .font(.caption.monospaced()).textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading)
                        }
                        .frame(maxHeight: 110)
                    }
                }
            }
            HStack(spacing: 10) {
                if running { ProgressView().controlSize(.small); Text("Working…").foregroundStyle(.secondary).font(.callout) }
                Spacer()
                Button(outcome == .done ? "Done" : "Cancel", action: close)
                    .buttonStyle(.quietPill)
                    .keyboardShortcut(.cancelAction)
                    .disabled(running)
                if outcome == nil || isFailure {
                    Button(isFailure ? "Try Again" : plan.actionLabel) { start() }
                        .buttonStyle(.pill)
                        .keyboardShortcut(.defaultAction)
                        .disabled(running)
                }
            }
        }
        .padding(24)
        .frame(width: 560)
        .tint(Theme.accent)
    }

    private var isFailure: Bool { if case .failed? = outcome { return true } else { return false } }

    private var symbol: String {
        switch plan.kind {
        case .enablePublic: return "globe"
        case .enablePrivate: return "lock.shield"
        case .disablePublic: return "globe.badge.chevron.backward"
        case .disablePrivate: return "xmark.shield"
        }
    }

    private var doneText: String {
        switch plan.resultingMode {
        case .publicFunnel: return "Guests can join from anywhere."
        case .privateTailnet: return "Guests on your Tailscale network can join."
        case .off: return "Only this Mac can open p2p.video."
        }
    }

    private func start() {
        running = true
        Task {
            outcome = await model.apply(plan)
            running = false
        }
    }
}

/// "Allow public links in Tailscale": the step shown when Tailscale printed
/// an enable link. Open Tailscale opens it in the default browser.
struct ApprovalCard: View {
    let approval: TailscaleApproval
    var busy = false
    let tryAgain: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Label {
                Text(approval.title).font(.headline)
            } icon: {
                Image(systemName: "hand.raised.fill").foregroundStyle(Theme.accent)
            }
            Text(approval.explanation)
                .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            Text("Click Open Tailscale, approve it on the page that opens, then come back and click Try Again.")
                .font(.callout).fixedSize(horizontal: false, vertical: true)
            HStack(spacing: 10) {
                Button {
                    NSWorkspace.shared.open(approval.url)
                } label: {
                    Label("Open Tailscale", systemImage: "arrow.up.forward.app")
                }
                .buttonStyle(.pill)
                Button("Try Again", action: tryAgain)
                    .buttonStyle(.quietPill)
                    .disabled(busy)
                if busy { ProgressView().controlSize(.small) }
            }
            .padding(.top, 2)
        }
        .padding(16)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 12, style: .continuous).fill(Theme.accentSoft))
        .overlay(RoundedRectangle(cornerRadius: 12, style: .continuous).strokeBorder(Theme.accent.opacity(0.35)))
        .accessibilityElement(children: .contain)
    }
}

/// The approval step outside the sheet (Settings → Guest Access and the
/// setup guide), for when the sheet was closed before approving.
struct PendingApprovalBanner: View {
    @ObservedObject var model: AppModel
    @State private var busy = false
    @State private var message: String?

    var body: some View {
        if let pending = model.tailscale.pendingApproval {
            VStack(alignment: .leading, spacing: 8) {
                ApprovalCard(approval: pending.approval, busy: busy) {
                    busy = true
                    Task {
                        switch await model.retryAfterApproval() {
                        case .failed(let m)?: message = m
                        case .needsApproval?: message = "Tailscale still needs your approval. Open Tailscale, approve, then try again."
                        default: message = nil
                        }
                        busy = false
                    }
                }
                if let message {
                    Text(message).font(.caption).foregroundStyle(.orange).fixedSize(horizontal: false, vertical: true)
                }
            }
        }
    }
}

struct CheckRow: View {
    let ok: Bool
    var optional = false
    let title: String
    let detail: String

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            Image(systemName: ok ? "checkmark.circle.fill" : (optional ? "circle.dashed" : "exclamationmark.circle.fill"))
                .foregroundStyle(ok ? .green : (optional ? .secondary : .orange))
            VStack(alignment: .leading, spacing: 2) {
                Text(title)
                Text(detail).font(.caption).foregroundStyle(.secondary).textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
            }
        }
        .accessibilityElement(children: .combine)
    }
}
