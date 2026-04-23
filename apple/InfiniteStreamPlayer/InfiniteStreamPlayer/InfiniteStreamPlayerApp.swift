import SwiftUI
import UIKit

private class AppDelegate: NSObject, UIApplicationDelegate {
    func applicationDidBecomeActive(_ application: UIApplication) {
        application.isIdleTimerDisabled = true
    }
    func applicationWillResignActive(_ application: UIApplication) {
        application.isIdleTimerDisabled = false
    }
}

@main
struct InfiniteStreamPlayerApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate

    var body: some Scene {
        WindowGroup {
            ContentView()
        }
    }
}
