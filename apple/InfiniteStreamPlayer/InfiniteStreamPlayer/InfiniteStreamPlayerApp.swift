import SwiftUI
import UIKit

private class AppDelegate: NSObject, UIApplicationDelegate {
    func application(
        _ application: UIApplication,
        didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil
    ) -> Bool {
        application.isIdleTimerDisabled = true
        return true
    }

    // Re-assert on every activation. On tvOS, AVPlayerViewController only
    // suppresses the screensaver during active playback — stalls, pauses,
    // and menu navigation let the system timer resume. Keep it disabled
    // app-wide so streaming sessions don't get interrupted.
    func applicationDidBecomeActive(_ application: UIApplication) {
        application.isIdleTimerDisabled = true
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
