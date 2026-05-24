// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "OrchidCapture",
    platforms: [.macOS(.v14)],
    products: [
        .executable(name: "OrchidCapture", targets: ["OrchidCapture"])
    ],
    dependencies: [
        // Global hotkey support — sandbox-safe, SwiftUI-native, lets the user
        // rebind via a recorder view. Wraps the Carbon RegisterEventHotKey API
        // so the prototype stays out of the deprecated Carbon Toolbox.
        // https://github.com/sindresorhus/KeyboardShortcuts
        .package(url: "https://github.com/sindresorhus/KeyboardShortcuts", from: "2.2.3"),
    ],
    targets: [
        .executableTarget(
            name: "OrchidCapture",
            dependencies: [
                .product(name: "KeyboardShortcuts", package: "KeyboardShortcuts"),
            ],
            path: "Sources/OrchidCapture"
        )
    ]
)
