import Foundation
import StudioLinkCore
import XCTest

enum Fixture {
    static func data(_ name: String) throws -> Data {
        guard let url = Bundle.module.url(forResource: name, withExtension: nil, subdirectory: "Fixtures") else {
            throw CocoaError(.fileNoSuchFile, userInfo: [NSFilePathErrorKey: name])
        }
        return try Data(contentsOf: url)
    }
}

let testResources = URL(fileURLWithPath: "/Applications/p2p.video.app/Contents/Resources", isDirectory: true)
let testHome = URL(fileURLWithPath: "/Users/tester", isDirectory: true)

/// A resolved configuration with a fixed build ID and optional env overrides.
func configuration(_ env: [String: String] = [:]) throws -> HostConfiguration {
    try HostConfiguration.resolve(environment: env, resources: testResources, home: testHome) { _ in "BUILD-1\n" }
}
