import Foundation

/// The first-run setup guide's steps, in order [SL-P2-2].
public enum OnboardingStep: Int, CaseIterable, Sendable, Comparable {
    case welcome, tailscale, guestAccess, browser, firstRoom, done

    public static func < (a: OnboardingStep, b: OnboardingStep) -> Bool { a.rawValue < b.rawValue }

    public var title: String {
        switch self {
        case .welcome: return "Welcome"
        case .tailscale: return "Connect Tailscale"
        case .guestAccess: return "How guests join"
        case .browser: return "Browser"
        case .firstRoom: return "First room"
        case .done: return "All set"
        }
    }
}

/// What the app currently knows, used to decide each step's status and
/// button labels. Built from live state; contains no secrets.
public struct OnboardingContext: Equatable, Sendable {
    public var daemonHealthy: Bool
    public var tailscaleInstalled: Bool
    public var tailscaleLoggedIn: Bool
    public var httpsAvailable: Bool
    public var funnelAllowed: Bool
    /// The verified Serve/Funnel route, or the saved mode when unknown.
    public var guestAccess: GuestAccessMode
    public var chromeInstalled: Bool
    public var roomCount: Int

    public init(daemonHealthy: Bool = false, tailscaleInstalled: Bool = false, tailscaleLoggedIn: Bool = false,
                httpsAvailable: Bool = false, funnelAllowed: Bool = false, guestAccess: GuestAccessMode = .off,
                chromeInstalled: Bool = false, roomCount: Int = 0) {
        self.daemonHealthy = daemonHealthy
        self.tailscaleInstalled = tailscaleInstalled
        self.tailscaleLoggedIn = tailscaleLoggedIn
        self.httpsAvailable = httpsAvailable
        self.funnelAllowed = funnelAllowed
        self.guestAccess = guestAccess
        self.chromeInstalled = chromeInstalled
        self.roomCount = roomCount
    }

    /// Tailscale is installed, signed in and can issue HTTPS certificates.
    public var tailscaleReady: Bool { tailscaleInstalled && tailscaleLoggedIn && httpsAvailable }
}

/// A guest-access choice offered by the guide. Each non-local choice runs the
/// existing consented command flow (exact commands shown, explicit click).
public enum GuestChoice: String, CaseIterable, Sendable {
    case privateTailnet, publicLink, later

    public var mode: GuestAccessMode? {
        switch self {
        case .privateTailnet: return .privateTailnet
        case .publicLink: return .publicFunnel
        case .later: return nil
        }
    }
}

public enum ChoiceAvailability: Equatable, Sendable {
    /// Already the active route.
    case current
    case available
    /// Runnable, but Tailscale may ask the owner to approve something first.
    case availableWithCaveat(String)
    case unavailable(String)

    public var selectable: Bool {
        switch self {
        case .available, .availableWithCaveat: return true
        case .current, .unavailable: return false
        }
    }
}

/// The guide's state machine. Every step can be skipped: nothing here blocks
/// the host from using p2p.video on this Mac.
public struct OnboardingFlow: Equatable, Sendable {
    public private(set) var step: OnboardingStep
    /// Set once the person reaches the end; persisted by the caller.
    public private(set) var completed: Bool

    public init(step: OnboardingStep = .welcome, completed: Bool = false) {
        self.step = step
        self.completed = completed
    }

    public var isFirst: Bool { step == OnboardingStep.allCases.first }
    public var isLast: Bool { step == OnboardingStep.allCases.last }
    /// 1-based position for "Step 2 of 6".
    public var position: Int { step.rawValue + 1 }
    public static var count: Int { OnboardingStep.allCases.count }

    public mutating func next() {
        if let n = OnboardingStep(rawValue: step.rawValue + 1) { step = n } else { completed = true }
    }

    public mutating func back() {
        if let p = OnboardingStep(rawValue: step.rawValue - 1) { step = p }
    }

    /// Jumps back to a step already passed (the sidebar). Forward jumps are
    /// ignored so each step is seen at least once.
    public mutating func go(to target: OnboardingStep) {
        if target <= step { step = target }
    }

    /// Finishing from the last step (or skipping the whole guide).
    public mutating func finish() {
        step = .done
        completed = true
    }

    // MARK: per-step decisions

    /// Is the step's goal already met?
    public static func satisfied(_ step: OnboardingStep, _ c: OnboardingContext) -> Bool {
        switch step {
        case .welcome: return true
        case .tailscale: return c.tailscaleReady
        case .guestAccess: return c.guestAccess != .off
        case .browser: return true
        case .firstRoom: return c.roomCount > 0
        case .done: return c.daemonHealthy
        }
    }

    /// The primary button's label for a step.
    public static func primaryLabel(_ step: OnboardingStep, _ c: OnboardingContext) -> String {
        switch step {
        case .welcome: return "Get Started"
        case .tailscale: return c.tailscaleReady ? "Continue" : "Skip for Now"
        case .guestAccess: return c.guestAccess == .off ? "Decide Later" : "Continue"
        case .browser: return "Continue"
        case .firstRoom: return c.roomCount > 0 ? "Continue" : "Skip"
        case .done: return c.daemonHealthy ? "Open p2p.video" : "Finish"
        }
    }

    /// Whether a guest-access choice can be picked right now, and why not.
    public static func availability(of choice: GuestChoice, _ c: OnboardingContext) -> ChoiceAvailability {
        guard let mode = choice.mode else { return .available }
        if c.guestAccess == mode { return .current }
        guard c.tailscaleInstalled else { return .unavailable("Connect Tailscale first (previous step).") }
        guard c.tailscaleLoggedIn else { return .unavailable("Sign in to Tailscale first (previous step).") }
        guard c.httpsAvailable else { return .unavailable("Turn on HTTPS in Tailscale first (previous step).") }
        if mode == .publicFunnel, !c.funnelAllowed {
            return .availableWithCaveat("The first time, Tailscale may ask you to allow public links. p2p.video shows you where.")
        }
        return .available
    }
}
