import Foundation
import UIKit

/// Caps the number of simultaneous live-preview AVPlayer decodes on the
/// Home screen. Apple doesn't expose a public concurrent-decoder count
/// (unlike Android's `MediaCodec.getMaxSupportedInstances`), so the
/// caps below are conservative defaults tuned from observation.
///
/// Tiles past the cap render the static poster + a `QUEUED` chip; they
/// retry acquisition automatically when a sibling tile scrolls offscreen
/// and releases its slot (the `revision` publish wakes every observer).
///
/// Mirrors the spirit of the Android `LeaseManager` decoder lease pool —
/// just simpler (no priority, no preemption; first-come, first-served
/// is fine because LazyHStack already mounts views roughly in scroll
/// order).
final class DecodeBudget: ObservableObject {
    static let shared = DecodeBudget()

    /// Hardware-derived ceiling. Picked per device class — high-end
    /// Apple TV 4K runs more concurrent previews than a base iPhone
    /// with a tighter thermal budget. Users can *lower* the actual cap
    /// via `setUserCap(_:)` (Settings → Advanced) but never raise it
    /// past this value.
    let hardwareCap: Int

    /// User's preferred slot count. 0 = preview video off entirely
    /// (every tile shows its thumbnail). Defaults to `hardwareCap` so
    /// first-launch users get the rich experience their device can
    /// handle. Use `setUserCap(_:)` to update — clamping + grant
    /// eviction live there to avoid the `@Published` + `didSet`
    /// re-entrancy headache that was making the +/- stepper look
    /// non-responsive.
    @Published private(set) var userCap: Int

    /// Bumped on every grant / release / userCap change so any tile
    /// observing this object re-evaluates whether it can now claim a
    /// slot.
    @Published private(set) var revision: Int = 0

    /// Grants stored in acquisition order — newest at the end. Used so
    /// eviction always frees the *oldest* tile rather than an
    /// arbitrary one (Set order is unstable). When the user scrolls,
    /// preview tiles claim slots in scroll-order; freeing the oldest
    /// drops whatever's been on screen longest, which lines up with
    /// what the user is most likely to have moved away from.
    private var grants: [UUID] = []

    private init() {
        #if os(tvOS)
        // Apple TV 4K (2nd gen +) handles 6× 360p H.264 decodes
        // comfortably. Older boxes auto-clamp via thermal throttling.
        hardwareCap = 6
        #else
        switch UIDevice.current.userInterfaceIdiom {
        case .pad:
            hardwareCap = 5
        case .phone:
            hardwareCap = 4
        default:
            hardwareCap = 4
        }
        #endif
        userCap = hardwareCap
    }

    /// Update the cap. Clamps to `[0, hardwareCap]` and evicts the
    /// oldest grants that exceed the new ceiling (keeping the newest
    /// `clamped` items) so a freshly-lowered setting takes effect
    /// immediately rather than waiting for tiles to scroll offscreen.
    func setUserCap(_ value: Int) {
        let clamped = max(0, min(value, hardwareCap))
        guard clamped != userCap || grants.count > clamped else { return }
        userCap = clamped
        if grants.count > clamped {
            grants = Array(grants.suffix(clamped))
        }
        revision &+= 1
    }

    /// Try to claim a decode slot for `id`. Returns true if a slot was
    /// granted (or was already held). False means the budget is full
    /// or the user has set the cap to 0 (preview video disabled).
    func acquire(_ id: UUID) -> Bool {
        if grants.contains(id) { return true }
        guard grants.count < userCap else { return false }
        grants.append(id)
        revision &+= 1
        return true
    }

    /// Release the slot for `id`. No-op if `id` doesn't hold one.
    func release(_ id: UUID) {
        if let idx = grants.firstIndex(of: id) {
            grants.remove(at: idx)
            revision &+= 1
        }
    }

    /// Whether `id` currently holds a slot. Tiles use this to detect
    /// when they've been evicted (priority allocation, see below) and
    /// flip their AVPlayer down to the static thumbnail.
    func hasGrant(_ id: UUID) -> Bool { grants.contains(id) }

    /// Priority claim — guaranteed to succeed when `userCap > 0`.
    /// Called when a tile becomes focused: the focused tile must
    /// always be decoding live (assuming the user has any slots
    /// configured at all).
    ///
    /// Eviction policy is **LRU**: every focus event moves the tile to
    /// the end of `grants` (most-recently-touched). The head of the
    /// array is the least-recently-focused tile — that's what gets
    /// evicted to make room for a focused tile that doesn't have a
    /// slot yet. So a tile that was decoded but never re-focused will
    /// be evicted before one the user just visited; a tile the user
    /// keeps coming back to keeps its slot indefinitely.
    @discardableResult
    func notifyFocused(_ id: UUID) -> Bool {
        if let existing = grants.firstIndex(of: id) {
            // Already has a grant — bump it to the end so it counts
            // as most-recently-focused for future eviction decisions.
            // Skip the move (and the revision bump) when it's already
            // at the tail — avoids spurious re-renders on every focus
            // refresh of the same tile.
            if existing != grants.count - 1 {
                grants.remove(at: existing)
                grants.append(id)
                revision &+= 1
            }
            return true
        }
        guard userCap > 0 else { return false }
        if grants.count >= userCap {
            // Evict the LRU (head) non-self grant.
            if let idx = grants.firstIndex(where: { $0 != id }) {
                grants.remove(at: idx)
            }
        }
        grants.append(id)
        revision &+= 1
        return true
    }
}
