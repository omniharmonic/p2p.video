import Foundation

/// Results of the WKWebView capability probe (see `WebViewProbe.swift`).
public struct WebViewCapabilityReport: Codable, Equatable, Sendable {
    public struct Check: Codable, Equatable, Sendable {
        public var name: String
        public var ok: Bool
        public var required: Bool
        public var detail: String

        public init(name: String, ok: Bool, required: Bool, detail: String) {
            self.name = name
            self.ok = ok
            self.required = required
            self.detail = detail
        }
    }

    public var checks: [Check]
    public var userAgent: String
    public var origin: String
    /// Whether WebKit called the native media-capture permission delegate.
    public var mediaPermissionDelegateCalled: Bool
    public var finishedAt: String

    public init(checks: [Check], userAgent: String, origin: String, mediaPermissionDelegateCalled: Bool, finishedAt: String) {
        self.checks = checks
        self.userAgent = userAgent
        self.origin = origin
        self.mediaPermissionDelegateCalled = mediaPermissionDelegateCalled
        self.finishedAt = finishedAt
    }

    /// Names the studio needs to host a call in-app (host role, recording).
    public static let requiredNames: [String] = [
        "secureContext", "getUserMedia", "mediaRecorder", "mediaRecorderStudioMime",
        "opfsGetDirectory", "opfsCreateWritable", "audioWorklet", "webSocket", "rtcPeerConnection",
    ]

    public var missingRequired: [String] {
        Self.requiredNames.filter { name in !(checks.first { $0.name == name }?.ok ?? false) }
    }

    /// The studio can run fully inside the app's web view.
    public var supportsInAppStudio: Bool { missingRequired.isEmpty }
}
