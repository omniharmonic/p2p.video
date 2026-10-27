import AppKit
import AVFoundation
import ObjectiveC
import StudioLinkCore
import WebKit

/// Automated WKWebView capability probe. Runs the probe page on the daemon's
/// loopback origin in a real (visible) window and writes a JSON report.
@MainActor
final class WebViewProbe: NSObject, WKScriptMessageHandler {
    private let port: Int
    private let output: URL
    private let uiDelegate: LoopbackMediaUIDelegate
    private var window: NSWindow?
    private var webView: WKWebView?
    private var completion: ((WebViewCapabilityReport?) -> Void)?
    private var timeout: Task<Void, Never>?

    private let mockDevices: Bool
    private var tccBefore: [String: String] = [:]
    private var privateSwitches: [String: Bool] = [:]

    /// - Parameter mockDevices: second pass only. Enables WebKit's private
    ///   mock capture devices, which bypass macOS camera/microphone (TCC)
    ///   authorization, to test WebKit's capture → MediaRecorder → WebRTC
    ///   plumbing when no person is present to answer the TCC prompt. Never
    ///   used outside the probe.
    init(port: Int, output: URL, mockDevices: Bool = false) {
        self.port = port
        self.output = output
        self.mockDevices = mockDevices
        self.uiDelegate = LoopbackMediaUIDelegate(port: port)
    }

    static func tccStatus() -> [String: String] {
        func name(_ s: AVAuthorizationStatus) -> String {
            switch s {
            case .notDetermined: return "notDetermined"
            case .restricted: return "restricted"
            case .denied: return "denied"
            case .authorized: return "authorized"
            @unknown default: return "unknown"
            }
        }
        return ["camera": name(AVCaptureDevice.authorizationStatus(for: .video)),
                "microphone": name(AVCaptureDevice.authorizationStatus(for: .audio))]
    }

    func run(completion: @escaping (WebViewCapabilityReport?) -> Void) {
        self.completion = completion
        tccBefore = Self.tccStatus()
        let view = StudioWebViewFactory.make(uiDelegate: uiDelegate)
        if mockDevices {
            privateSwitches["mockCaptureDevices"] = Self.callPrivateBoolSetter(view.configuration.preferences, "_setMockCaptureDevicesEnabled:", true)
        }
        // WebKit defers capture (and throttles hidden pages) while the window is
        // occluded, e.g. with the screen locked during an unattended run. The
        // probe disables occlusion detection so results reflect capability,
        // not visibility. Probe-only private switch.
        privateSwitches["occlusionDetectionDisabled"] = Self.callPrivateBoolSetter(view, "_setWindowOcclusionDetectionEnabled:", false)
        view.configuration.userContentController.add(self, name: "probe")
        let window = NSWindow(contentRect: NSRect(x: 80, y: 80, width: 720, height: 560), styleMask: [.titled, .closable],
                              backing: .buffered, defer: false)
        window.title = "p2p.video WKWebView probe"
        window.contentView = view
        window.isReleasedWhenClosed = false
        window.makeKeyAndOrderFront(nil)
        self.window = window
        self.webView = view
        view.loadHTMLString(ProbePage.html, baseURL: URL(string: "http://127.0.0.1:\(port)/")!)
        timeout = Task { [weak self] in
            try? await Task.sleep(for: .seconds(90))
            self?.finish(json: nil, note: "probe timed out after 90 s")
        }
    }

    nonisolated func userContentController(_ controller: WKUserContentController, didReceive message: WKScriptMessage) {
        // WebKit delivers script messages on the main thread.
        MainActor.assumeIsolated { self.finish(json: message.body as? String, note: nil) }
    }

    private func finish(json: String?, note: String?) {
        guard let completion else { return }
        self.completion = nil
        timeout?.cancel()
        struct Payload: Decodable { var checks: [WebViewCapabilityReport.Check]; var userAgent: String; var origin: String }
        var report: WebViewCapabilityReport?
        if let json, let data = json.data(using: .utf8), let p = try? JSONDecoder().decode(Payload.self, from: data) {
            report = WebViewCapabilityReport(checks: p.checks, userAgent: p.userAgent, origin: p.origin,
                                             mediaPermissionDelegateCalled: uiDelegate.permissionRequests > 0,
                                             finishedAt: ISO8601DateFormatter().string(from: Date()))
        }
        var doc: [String: Any] = ["note": note ?? "", "permissionDelegateCalls": uiDelegate.permissionRequests,
                                  "mockCaptureDevices": mockDevices, "tccBefore": tccBefore, "tccAfter": Self.tccStatus(),
                                  "privateSwitchesApplied": privateSwitches]
        if let report, let data = try? JSONEncoder().encode(report), let obj = try? JSONSerialization.jsonObject(with: data) {
            doc["report"] = obj
            doc["missingRequired"] = report.missingRequired
            doc["supportsInAppStudio"] = report.supportsInAppStudio
        }
        if let data = try? JSONSerialization.data(withJSONObject: doc, options: [.prettyPrinted, .sortedKeys]) {
            try? FileManager.default.createDirectory(at: output.deletingLastPathComponent(), withIntermediateDirectories: true)
            try? data.write(to: output, options: .atomic)
        }
        webView?.configuration.userContentController.removeScriptMessageHandler(forName: "probe")
        window?.close()
        completion(report)
    }

    /// Calls a private Bool setter (`_setX:`) if the object implements it.
    @discardableResult
    private static func callPrivateBoolSetter(_ object: NSObject, _ selector: String, _ value: Bool) -> Bool {
        let sel = NSSelectorFromString(selector)
        guard object.responds(to: sel), let method = class_getInstanceMethod(type(of: object), sel) else {
            appLog.error("Probe: \(selector, privacy: .public) unavailable")
            return false
        }
        typealias Setter = @convention(c) (AnyObject, Selector, Bool) -> Void
        unsafeBitCast(method_getImplementation(method), to: Setter.self)(object, sel, value)
        return true
    }
}
