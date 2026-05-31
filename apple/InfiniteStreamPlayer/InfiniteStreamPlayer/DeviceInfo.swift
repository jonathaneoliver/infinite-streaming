// #550 Phase 4 — device / platform / version taxonomy.
//
// One canonical source for the device fields we stamp on every
// session_events row. Computed lazily once per process; values don't
// change across the app's lifetime so memoising is safe and cheap.
//
// Mirrors the Conviva / Bitmovin breakout: collector identity (the
// "platform" string already in use, e.g. "ios"/"tvos") stays separate
// from hardware class (`device_class`) and physical model
// (`device_model`). `player_tech` names the playback engine so a
// future hls.js / shaka / Roku build can A/B against AVPlayer on the
// same device by varying just that field.

import Foundation
#if canImport(UIKit)
import UIKit
#endif

enum DeviceInfo {
    /// iOS / tvOS / iPadOS major version, e.g. 26 for "26.0.1".
    static let osVersionMajor: UInt16 = parsedOSVersion.major

    /// Minor version, e.g. 0 for "26.0.1". Patch version is dropped
    /// at the schema level (LowCardinality-friendly cardinality).
    static let osVersionMinor: UInt16 = parsedOSVersion.minor

    /// App marketing version from Bundle (CFBundleShortVersionString).
    /// Synced to the repo-root VERSION file by the "Sync version from
    /// repo VERSION" Xcode build phase before every compile — single
    /// source of truth shared with the Android build.gradle, the
    /// server image tag, and the dashboard "What's New" banner. To
    /// bump, edit /VERSION at the repo root; the next build picks
    /// it up automatically and writes both Info.plist files.
    static let appVersion: String = {
        let dict = Bundle.main.infoDictionary
        return (dict?["CFBundleShortVersionString"] as? String) ?? ""
    }()

    /// Form-factor enum: "phone" / "tablet" / "tv" / "desktop" /
    /// "unknown". Derived from UIUserInterfaceIdiom on Apple
    /// platforms; "desktop" reserved for future macOS builds; empty
    /// when UIKit is unavailable.
    static let deviceClass: String = {
        #if canImport(UIKit)
        switch UIDevice.current.userInterfaceIdiom {
        case .phone:        return "phone"
        case .pad:          return "tablet"
        case .tv:           return "tv"
        case .mac:          return "desktop"
        case .carPlay:      return "carplay"
        case .vision:       return "vision"
        case .unspecified:  return "unknown"
        @unknown default:   return "unknown"
        }
        #else
        return "unknown"
        #endif
    }()

    /// Hardware model identifier — e.g. "iPhone15,3" or
    /// "AppleTV14,1". Read via sysctl("hw.machine") because UIDevice
    /// only exposes a generic "iPhone" / "iPad" label.
    static let deviceModel: String = {
        var size = 0
        sysctlbyname("hw.machine", nil, &size, nil, 0)
        guard size > 0 else { return "" }
        var buf = [CChar](repeating: 0, count: size)
        sysctlbyname("hw.machine", &buf, &size, nil, 0)
        return String(cString: buf)
    }()

    /// Playback engine identifier. Hard-coded "AVPlayer" for this
    /// Apple-platform build; a future cross-platform build would set
    /// it to "hls.js" / "shaka" / "native-roku" etc.
    static let playerTech: String = "AVPlayer"

    /// Physical-pixel resolution of the device's current orientation,
    /// formatted as `"WxH"` to match `video_resolution` /
    /// `display_resolution` for side-by-side comparison. On iPad this
    /// swaps when the device rotates; on Apple TV / iPhone-locked
    /// orientations it stays constant. Computed each call so the
    /// caller gets the current orientation rather than the value at
    /// app launch.
    ///
    /// Replaces the prior `screen_width_px` / `screen_height_px` /
    /// `screen_density` taxonomy fields, which were static
    /// portrait-only readings and never reflected rotation.
    static func deviceResolution() -> String {
        #if canImport(UIKit)
        let b = UIScreen.main.bounds
        let s = UIScreen.main.nativeScale
        let w = Int((b.width  * s).rounded())
        let h = Int((b.height * s).rounded())
        guard w > 0 && h > 0 else { return "" }
        return "\(w)x\(h)"
        #else
        return ""
        #endif
    }

    // ── Helpers ────────────────────────────────────────────────────

    private static let parsedOSVersion: (major: UInt16, minor: UInt16) = {
        let v = ProcessInfo.processInfo.operatingSystemVersion
        let major = (v.majorVersion >= 0 && v.majorVersion <= Int(UInt16.max))
            ? UInt16(v.majorVersion) : 0
        let minor = (v.minorVersion >= 0 && v.minorVersion <= Int(UInt16.max))
            ? UInt16(v.minorVersion) : 0
        return (major, minor)
    }()
}
