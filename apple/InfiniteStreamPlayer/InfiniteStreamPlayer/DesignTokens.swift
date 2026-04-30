import SwiftUI

// Design tokens shared by every screen — typography, colour, spacing,
// radii, motion. Mirrors the Android TV `Tokens` / `AppType` / `Space` /
// `Radius` / `Motion` set, kept in lockstep so iPhone, iPad, and Apple
// TV match the cinematic UI defined for issue #251.
//
// We bias toward system fonts here rather than wiring up the
// downloadable-fonts service Android uses (Fraunces / Inter Tight /
// JetBrains Mono). Apple's system serif (`.serif`) and rounded /
// monospaced design variants give us the same tonal contrast — title
// serif, body sans, caption mono — without bundling another font file.

enum Tokens {
    // Surfaces — almost-black gradient background to deep-charcoal cards.
    // Matches Android TV `bg`, `bgSoft`, `bgCard`.
    static let bg = Color(red: 0.04, green: 0.04, blue: 0.05)
    static let bgSoft = Color(red: 0.07, green: 0.07, blue: 0.09)
    static let bgCard = Color(red: 0.10, green: 0.10, blue: 0.13)

    // Text — high-contrast white tier, then progressively dimmer.
    static let fg = Color(white: 0.98)
    static let fgDim = Color(white: 0.70)
    static let fgFaint = Color(white: 0.45)

    // Hairline divider colour for borders.
    static let line = Color.white.opacity(0.08)

    // Brand accents.
    /// Warm gold for primary focus / selection / chrome highlights.
    static let accent = Color(red: 0.95, green: 0.74, blue: 0.20)
    /// Coral red used exclusively for the LIVE badge — never for focus.
    static let live = Color(red: 0.98, green: 0.30, blue: 0.34)
    /// Cool blue reserved for diagnostic / dev-mode overlays.
    static let diag = Color(red: 0.36, green: 0.74, blue: 0.95)
    /// Destructive red used for irreversible actions ("Reset All
    /// Settings", "Forget Server"). Distinct from `live` (the playback
    /// LIVE badge) so the two reds don't read as the same affordance.
    static let destructive = Color(red: 0.92, green: 0.30, blue: 0.30)
}

enum Space {
    static let s1: CGFloat = 4
    static let s2: CGFloat = 8
    static let s3: CGFloat = 12
    static let s4: CGFloat = 16
    static let s5: CGFloat = 24
    static let s6: CGFloat = 32
    static let s7: CGFloat = 48
    static let s8: CGFloat = 64
}

enum Radius {
    static let row: CGFloat = 10
    static let card: CGFloat = 14
    static let panel: CGFloat = 18
}

enum Motion {
    /// Drawer slide-in / slide-out animation duration.
    static let drawerS: Double = 0.24
    /// Focus / press scale-up duration.
    static let focusS: Double = 0.12
}

enum AppType {
    // Title — serif for the cinematic "now playing" hero text. Apple's
    // system serif is closer to Fraunces in spirit than the rounded
    // default and reads well on a TV from 10 feet.
    static func title(size: CGFloat = 34) -> Font {
        .system(size: size, weight: .semibold, design: .serif)
    }
    static func titleSm(size: CGFloat = 24) -> Font {
        .system(size: size, weight: .semibold, design: .serif)
    }

    // Body — geometric sans, the default. Mirrors Inter Tight.
    static func body(size: CGFloat = 17) -> Font {
        .system(size: size, weight: .regular, design: .default)
    }
    static func bodyEm(size: CGFloat = 17) -> Font {
        .system(size: size, weight: .semibold, design: .default)
    }

    // Mono — for value text on settings rows and any URL / id readout.
    static func mono(size: CGFloat = 15) -> Font {
        .system(size: size, weight: .regular, design: .monospaced)
    }
    static func monoSm(size: CGFloat = 12) -> Font {
        .system(size: size, weight: .medium, design: .monospaced)
    }

    // Label — small all-caps label-style text used over hero rows
    // ("NOW PLAYING", "CONTINUE WATCHING").
    static func label(size: CGFloat = 12) -> Font {
        .system(size: size, weight: .bold, design: .default)
    }
}

// MARK: - Focus styling

/// 3px gold ring + 1.04× scale when focused (tvOS) or pressed (iOS/iPadOS).
/// Matches the Android `tvFocus` modifier — always reach for this rather
/// than rolling per-call shadows / outlines.
///
/// Apply to **non-Button** views (raw HStacks with `.onTapGesture`).
/// `.focusable(true)` makes them focus-eligible. For SwiftUI `Button`s,
/// use `cinematicFocusFollower` instead — `.focusable(true)` would
/// replace the Button's interactive focus and silently kill the tap.
struct CinematicFocusModifier: ViewModifier {
    let cornerRadius: CGFloat
    /// Optional callback fired when the wrapped view's focus state
    /// changes on tvOS. Live preview tiles use this to ask the decode
    /// budget for a priority slot (evicting a sibling if necessary).
    var onFocusChange: ((Bool) -> Void)? = nil
    @State private var isFocused: Bool = false

    func body(content: Content) -> some View {
        let active = isFocused
        // Order matters: overlay first, then scaleEffect. Otherwise
        // the scaled content visually extends 4% past the ring's frame
        // and the corners look mis-aligned (especially on the larger
        // hero panel on Home).
        return content
            .overlay(
                RoundedRectangle(cornerRadius: cornerRadius, style: .continuous)
                    .stroke(active ? Tokens.accent : Color.clear, lineWidth: 3)
            )
            .scaleEffect(active ? 1.04 : 1.0)
            .animation(.easeOut(duration: Motion.focusS), value: isFocused)
            #if os(tvOS)
            .focusable(true) { focused in
                isFocused = focused
                onFocusChange?(focused)
            }
            #endif
    }
}

/// Visual-only twin of `cinematicFocus` for use **inside a Button's
/// label**. Reads focus from `@Environment(\.isFocused)` (set by the
/// enclosing Button) — does not add any focus modifier of its own, so
/// the Button's tap action is preserved.
struct CinematicFocusFollowerModifier: ViewModifier {
    let cornerRadius: CGFloat
    @Environment(\.isFocused) private var isFocused

    func body(content: Content) -> some View {
        // overlay before scaleEffect, same reasoning as CinematicFocusModifier.
        content
            .overlay(
                RoundedRectangle(cornerRadius: cornerRadius, style: .continuous)
                    .stroke(isFocused ? Tokens.accent : Color.clear, lineWidth: 3)
            )
            .scaleEffect(isFocused ? 1.04 : 1.0)
            .animation(.easeOut(duration: Motion.focusS), value: isFocused)
    }
}

/// Row treatment for Settings list / picker rows. Visually identical
/// to `cinematicFocus` (3px gold ring + 1.04× scale) but reads its
/// focus state from `@Environment(\.isFocused)` so it works inside a
/// SwiftUI Button label without doubling up the focusable. Use this
/// in `Button { ... } label: { row.cinematicRowStyle(...) }`.
struct CinematicRowStyleModifier: ViewModifier {
    let cornerRadius: CGFloat
    @Environment(\.isFocused) private var isFocused

    func body(content: Content) -> some View {
        content
            .background(Tokens.bgSoft)
            .clipShape(RoundedRectangle(cornerRadius: cornerRadius, style: .continuous))
            .overlay(
                RoundedRectangle(cornerRadius: cornerRadius, style: .continuous)
                    .stroke(isFocused ? Tokens.accent : Color.clear, lineWidth: 3)
            )
            .scaleEffect(isFocused ? 1.04 : 1.0)
            .animation(.easeOut(duration: Motion.focusS), value: isFocused)
    }
}

extension View {
    /// Apply the cinematic 3px-ring + 1.04× scale focus treatment.
    /// Pass the corner radius matching the underlying card / row.
    /// Use on raw views with `.onTapGesture`. Inside a `Button` label,
    /// use `cinematicFocusFollower` instead.
    ///
    /// `onFocusChange` is called on tvOS when the wrapped view gains
    /// or loses focus. `LivePreviewTile` uses it to claim a priority
    /// decode slot from `DecodeBudget` so the highlighted card always
    /// shows live video, even if a sibling has to give up its slot.
    func cinematicFocus(
        cornerRadius: CGFloat = Radius.row,
        onFocusChange: ((Bool) -> Void)? = nil
    ) -> some View {
        modifier(CinematicFocusModifier(
            cornerRadius: cornerRadius,
            onFocusChange: onFocusChange
        ))
    }

    /// Visual focus treatment that reads focus from the enclosing
    /// `Button`. Apply *inside* the Button's label so the Button stays
    /// the focusable + tap target.
    func cinematicFocusFollower(cornerRadius: CGFloat = Radius.row) -> some View {
        modifier(CinematicFocusFollowerModifier(cornerRadius: cornerRadius))
    }

    /// Row-style focus treatment — brighter background fill, 2px ring,
    /// gentler 1.02× scale. Use *inside* a Button's label on flat
    /// settings / picker rows. Replaces the
    /// `bgSoft + clipShape + cinematicFocusFollower` chain.
    func cinematicRowStyle(cornerRadius: CGFloat = Radius.row) -> some View {
        modifier(CinematicRowStyleModifier(cornerRadius: cornerRadius))
    }

    /// Wrap the receiver in a tvOS focus section so D-pad navigation
    /// stays contained within it (Right / Left within the section
    /// reach every focusable child instead of escaping to neighbouring
    /// views). No-op on iOS / iPadOS where there's no focus engine.
    @ViewBuilder
    func tvFocusSection() -> some View {
        #if os(tvOS)
        self.focusSection()
        #else
        self
        #endif
    }
}

/// Reusable circular `<` back button — same look across every screen
/// that needs a back affordance (Server picker, Settings drawer, Home,
/// Playback overlay). Uses `chevron.backward` so iPhone, iPad, and
/// tvOS all render the same glyph.
struct BackChevronButton: View {
    var weight: Font.Weight = .semibold
    var size: CGFloat = 18
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            Image(systemName: "chevron.backward")
                .font(.system(size: size, weight: weight))
                .foregroundColor(Tokens.fg)
                .padding(12)
                .background(Tokens.bgSoft)
                .clipShape(Circle())
                .cinematicFocusFollower(cornerRadius: 24)
        }
        .buttonStyle(.plain)
        #if os(tvOS)
        // Suppress AVKit / SwiftUI's default white-fill focus highlight
        // on tvOS so it doesn't compete with our cinematic gold ring.
        .focusEffectDisabled()
        #endif
    }
}
