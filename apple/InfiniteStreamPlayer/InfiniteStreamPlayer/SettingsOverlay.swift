import SwiftUI

/// Slide-from-right settings drawer. Spec from `BEHAVIOR.md`:
///   - 46% panel width on tvOS / iPad, 100% on iPhone (full-width sheet).
///   - 240ms enter/exit slide + fade, backdrop dim.
///   - Single Back semantics: pop picker → main list → close drawer.
///   - Sticky picker focus: returning re-focuses the row that opened it.
///
/// Mirrors the Android `SettingsOverlay` Composable.
struct SettingsOverlay: View {
    @ObservedObject var vm: PlayerViewModel
    let onOpenServerPicker: () -> Void

    @State private var picker: PickerKind?
    @Environment(\.horizontalSizeClass) private var hSizeClass
    /// tvOS focus scope for the drawer — used to direct initial focus
    /// onto the panel (rather than the left-side focus catcher) and
    /// keep escape attempts inside the scope.
    @Namespace private var drawerScope

    enum PickerKind: Hashable { case stream, proto, segment, codec, advanced }

    /// Compact width = iPhone in portrait (and iPad Slide Over). The
    /// drawer is full-width on compact, so we tighten everything to
    /// fit the row list above the keyboard / bottom safe area.
    private var isCompact: Bool { hSizeClass == .compact }
    private var panelHPadding: CGFloat { isCompact ? Space.s4 : Space.s7 }
    private var panelVPadding: CGFloat { isCompact ? Space.s4 : Space.s7 }
    private var titleSize: CGFloat { isCompact ? 22 : 28 }
    private var headerToBodyGap: CGFloat { isCompact ? Space.s3 : Space.s7 }

    var body: some View {
        // GeometryReader so the drawer width tracks the live window
        // size — rotating the iPad updates `geo.size.width` whereas
        // `UIScreen.main.bounds.width` stays at the natural-orientation
        // (portrait) value and leaves the drawer the wrong width after
        // rotation.
        GeometryReader { geo in
            ZStack {
                if vm.settingsOpen {
                    // Backdrop dim — tap to dismiss, swipe-back also
                    // handled via the OS back gesture on iOS.
                    LinearGradient(
                        colors: [.clear, Color.black.opacity(0.55)],
                        startPoint: .leading, endPoint: .trailing
                    )
                    .ignoresSafeArea()
                    .contentShape(Rectangle())
                    .onTapGesture { vm.setSettingsOpen(false) }
                    .transition(.opacity)

                    HStack(spacing: 0) {
                        Spacer()
                        panel
                            .frame(maxWidth: panelWidth(for: geo.size.width), maxHeight: .infinity)
                            .background(Tokens.bg)
                            .overlay(
                                Rectangle().stroke(Tokens.line, lineWidth: 1)
                            )
                            .tvFocusSection()
                            #if os(tvOS)
                            .prefersDefaultFocus(true, in: drawerScope)
                            #endif
                    }
                    .transition(.move(edge: .trailing).combined(with: .opacity))
                    #if os(tvOS)
                    .focusScope(drawerScope)
                    // Siri Remote Menu / Back: pop picker → main list →
                    // close drawer. Without this the user could only
                    // back out via the chevron at the top of the drawer.
                    .onExitCommand { handleBack() }
                    #endif
                }
            }
            .animation(.easeOut(duration: Motion.drawerS), value: vm.settingsOpen)
            .animation(.easeOut(duration: Motion.drawerS), value: picker)
        }
        .ignoresSafeArea()
    }

    /// Same slide-from-right drawer layout on every platform.
    /// 46% width on iPad / Apple TV (regular size class), 78% on
    /// iPhone (compact size class) so the user still sees a strip of
    /// the previous screen behind the dim. Single shape across the
    /// codebase — fonts and spacing get compact-mode shrinkage above
    /// rather than the drawer itself becoming a full-screen sheet.
    private func panelWidth(for screenWidth: CGFloat) -> CGFloat {
        return isCompact ? screenWidth * 0.78 : screenWidth * 0.46
    }

    private var panel: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: Space.s3) {
                BackChevronButton { handleBack() }
                Spacer()
            }
            Spacer().frame(height: isCompact ? Space.s2 : Space.s4)
            Text("NOW PLAYING")
                .font(AppType.label())
                .foregroundColor(Tokens.fgDim)
            Spacer().frame(height: Space.s1)
            Text(vm.selectedContent.isEmpty ? "—" : ContentItem.displayName(from: vm.selectedContent))
                .font(AppType.title(size: titleSize))
                .foregroundColor(Tokens.fg)
                .lineLimit(2)
            Spacer().frame(height: headerToBodyGap)

            Group {
                if let kind = picker {
                    PickerList(
                        kind: kind,
                        vm: vm,
                        compact: isCompact,
                        onBack: { picker = nil },
                        // After a Reset All Settings the app needs to
                        // route back to ServerPicker (no servers left).
                        // Reuse the same callback the Server row uses.
                        onResetComplete: onOpenServerPicker
                    )
                } else {
                    MainList(
                        vm: vm,
                        compact: isCompact,
                        onPick: { kind in picker = kind },
                        onOpenServerPicker: onOpenServerPicker
                    )
                }
            }
            .frame(maxHeight: .infinity, alignment: .top)

        }
        .padding(.horizontal, panelHPadding)
        .padding(.vertical, panelVPadding)
    }

    private func handleBack() {
        if picker != nil { picker = nil } else { vm.setSettingsOpen(false) }
    }
}

// MARK: - Main list

private struct MainList: View {
    @ObservedObject var vm: PlayerViewModel
    let compact: Bool
    let onPick: (SettingsOverlay.PickerKind) -> Void
    let onOpenServerPicker: () -> Void

    /// tvOS focus seed — set to 0 on appear so the first row receives
    /// focus when the drawer first opens. Without this AVPlayerViewController
    /// keeps focus and the drawer renders with no highlighted row.
    @FocusState private var rowIdx: Int?

    var body: some View {
        ScrollView {
            VStack(spacing: Space.s1) {
                SettingRow(label: "Server",
                           value: vm.activeServer?.label ?? "—",
                           compact: compact,
                           onTap: onOpenServerPicker)
                    .focused($rowIdx, equals: 0)
                SettingRow(label: "Stream",
                           value: vm.selectedContent.isEmpty ? "—" : ContentItem.displayName(from: vm.selectedContent),
                           compact: compact,
                           onTap: { onPick(.stream) })
                    .focused($rowIdx, equals: 1)
                SettingRow(label: "Protocol",
                           value: vm.streamProtocol.label,
                           compact: compact,
                           onTap: { onPick(.proto) })
                    .focused($rowIdx, equals: 2)
                SettingRow(label: "Segment length",
                           value: vm.segment.label,
                           compact: compact,
                           onTap: { onPick(.segment) })
                    .focused($rowIdx, equals: 3)
                SettingRow(label: "Codec",
                           value: vm.codec.label,
                           compact: compact,
                           onTap: { onPick(.codec) })
                    .focused($rowIdx, equals: 4)
                SettingRow(label: "Advanced",
                           value: "",
                           compact: compact,
                           onTap: { onPick(.advanced) })
                    .focused($rowIdx, equals: 5)
            }
        }
        .onAppear {
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.1) {
                rowIdx = 0
            }
        }
    }
}

private struct SettingRow: View {
    let label: String
    let value: String
    let compact: Bool
    let onTap: () -> Void

    var body: some View {
        HStack {
            Text(label)
                .font(compact ? AppType.body(size: 15) : AppType.body())
                .foregroundColor(Tokens.fg)
            Spacer()
            if !value.isEmpty {
                Text(value)
                    .font(compact ? AppType.mono(size: 13) : AppType.mono())
                    .foregroundColor(Tokens.fgDim)
                    .lineLimit(1)
            }
            Image(systemName: "chevron.right")
                .foregroundColor(Tokens.fgDim)
        }
        .padding(.horizontal, compact ? Space.s3 : Space.s4)
        .frame(height: compact ? 42 : 56)
        .background(Tokens.bgSoft)
        .clipShape(RoundedRectangle(cornerRadius: Radius.row, style: .continuous))
        .cinematicFocus(cornerRadius: Radius.row)
        .contentShape(Rectangle())
        .onTapGesture(perform: onTap)
    }
}

// MARK: - Picker pages

private struct PickerList: View {
    let kind: SettingsOverlay.PickerKind
    @ObservedObject var vm: PlayerViewModel
    let compact: Bool
    let onBack: () -> Void
    /// Called after Reset All Settings finishes wiping state, so the
    /// caller can re-route the app to ServerPicker (the empty-servers
    /// path AppRoot would normally take on first launch).
    let onResetComplete: () -> Void

    /// tvOS focus seed — set to 0 on appear so the first picker item
    /// receives focus when the page enters. Same reason as `MainList.rowIdx`.
    @FocusState private var itemIdx: Int?

    /// Confirmation alert state for the destructive Reset All Settings
    /// row. Local to the picker — no need to round-trip through the VM.
    @State private var showResetConfirm: Bool = false

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Text(headerLabel)
                .font(compact ? AppType.titleSm(size: 18) : AppType.titleSm())
                .foregroundColor(Tokens.fg)
            Spacer().frame(height: compact ? Space.s2 : Space.s4)
            ScrollView {
                VStack(spacing: Space.s1) {
                    pickerItems
                }
            }
        }
        .onAppear {
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.1) {
                itemIdx = 0
            }
        }
        // Confirmation for the destructive Reset All Settings row.
        // .alert renders natively on iOS / iPadOS / tvOS, no extra
        // platform branching needed.
        .alert("Reset All Settings?", isPresented: $showResetConfirm) {
            Button("Cancel", role: .cancel) { }
            Button("Reset", role: .destructive) {
                vm.resetAllSettings()
                onResetComplete()
            }
        } message: {
            Text("This will forget all saved servers and return the app to its first-launch state. Downloaded content and account data are unaffected.")
        }
    }

    private var headerLabel: String {
        switch kind {
        case .stream: return "Stream"
        case .proto: return "Protocol"
        case .segment: return "Segment length"
        case .codec: return "Codec"
        case .advanced: return "Advanced"
        }
    }

    @ViewBuilder
    private var pickerItems: some View {
        switch kind {
        case .stream:
            ForEach(Array(vm.filteredContent.enumerated()), id: \.element.id) { idx, item in
                PickerItem(label: item.displayName,
                           selected: item.name == vm.selectedContent,
                           compact: compact) {
                    vm.setSelectedContent(item.name); onBack()
                }
                .focused($itemIdx, equals: idx)
            }
        case .proto:
            ForEach(Array(StreamProtocol.allCases.enumerated()), id: \.element.id) { idx, p in
                PickerItem(label: p.label, selected: p == vm.streamProtocol, compact: compact) {
                    vm.setProtocol(p); onBack()
                }
                .focused($itemIdx, equals: idx)
            }
        case .segment:
            ForEach(Array(SegmentLength.allCases.enumerated()), id: \.element.id) { idx, s in
                PickerItem(label: s.label, selected: s == vm.segment, compact: compact) {
                    vm.setSegment(s); onBack()
                }
                .focused($itemIdx, equals: idx)
            }
        case .codec:
            ForEach(Array(CodecFilter.allCases.enumerated()), id: \.element.id) { idx, c in
                PickerItem(label: c.label, selected: c == vm.codec, compact: compact) {
                    vm.setCodec(c); onBack()
                }
                .focused($itemIdx, equals: idx)
            }
        case .advanced:
            ToggleRow(label: "4K (allow >1080p)",
                      isOn: vm.allow4K, compact: compact) { vm.setAllow4K($0) }
                .focused($itemIdx, equals: 0)
            ToggleRow(label: "Local Proxy",
                      isOn: vm.localProxy, compact: compact) { vm.setLocalProxy($0) }
                .focused($itemIdx, equals: 1)
            ToggleRow(label: "Auto-Recovery",
                      isOn: vm.autoRecovery, compact: compact) { vm.setAutoRecovery($0) }
                .focused($itemIdx, equals: 2)
            ToggleRow(label: "Go Live",
                      isOn: vm.goLive, compact: compact) { vm.setGoLive($0) }
                .focused($itemIdx, equals: 3)
            LiveOffsetRow(seconds: vm.liveOffsetSeconds, compact: compact) {
                vm.setLiveOffsetSeconds($0)
            }
            .focused($itemIdx, equals: 4)
            ToggleRow(label: "Skip Home on launch",
                      isOn: vm.skipHomeOnLaunch, compact: compact) { vm.setSkipHomeOnLaunch($0) }
                .focused($itemIdx, equals: 5)
            ToggleRow(label: "Mute audio",
                      isOn: vm.isMuted, compact: compact) { vm.setIsMuted($0) }
                .focused($itemIdx, equals: 6)
            PreviewVideoSlotsRow(slots: vm.previewVideoSlots,
                                 hardwareCap: DecodeBudget.shared.hardwareCap,
                                 compact: compact) {
                vm.setPreviewVideoSlots($0)
            }
            .focused($itemIdx, equals: 7)
            ToggleRow(label: "Developer mode",
                      isOn: vm.developerMode, compact: compact) { vm.setDeveloperMode($0) }
                .focused($itemIdx, equals: 8)
            DestructiveRow(label: "Reset All Settings", compact: compact) {
                showResetConfirm = true
            }
            .focused($itemIdx, equals: 9)
        }
    }
}

private struct PickerItem: View {
    let label: String
    let selected: Bool
    let compact: Bool
    let onTap: () -> Void

    var body: some View {
        HStack {
            Text(label)
                .font(compact ? AppType.body(size: 15) : AppType.body())
                .foregroundColor(Tokens.fg)
                .lineLimit(1)
            Spacer()
            if selected {
                Image(systemName: "checkmark")
                    .foregroundColor(Tokens.accent)
            }
        }
        .padding(.horizontal, compact ? Space.s3 : Space.s4)
        .frame(height: compact ? 38 : 48)
        .background(selected ? Tokens.bgCard : Tokens.bgSoft)
        .clipShape(RoundedRectangle(cornerRadius: Radius.row, style: .continuous))
        .cinematicFocus(cornerRadius: Radius.row)
        .contentShape(Rectangle())
        .onTapGesture(perform: onTap)
    }
}

/// Numeric stepper for the user-configurable live-edge offset (seconds).
/// 0 = use manifest's HOLD-BACK / Go Live default. Mirrors the
/// `live_offset_s` query param on `testing-session.html`.
private struct LiveOffsetRow: View {
    let seconds: Double
    let compact: Bool
    let onChange: (Double) -> Void

    var body: some View {
        HStack {
            VStack(alignment: .leading, spacing: 2) {
                Text("Live offset (seconds behind live)")
                    .font(compact ? AppType.body(size: 15) : AppType.body())
                    .foregroundColor(Tokens.fg)
                    .lineLimit(1)
                Text(seconds == 0 ? "Off — use manifest HOLD-BACK" : "\(Int(seconds))s behind live edge")
                    .font(AppType.monoSm())
                    .foregroundColor(Tokens.fgFaint)
            }
            Spacer()
            // Custom +/− pair instead of SwiftUI Stepper because Stepper
            // isn't available on tvOS. Same shape works across iPhone,
            // iPad, and Apple TV (D-pad / Siri Remote land on each
            // button via the focus engine).
            HStack(spacing: Space.s2) {
                Button { onChange(max(0, seconds - 1)) } label: {
                    Image(systemName: "minus")
                        .font(.system(size: 16, weight: .semibold))
                        .foregroundColor(Tokens.fg)
                        .frame(width: 36, height: 36)
                        .background(Tokens.bgCard)
                        .clipShape(Circle())
                        .cinematicFocusFollower(cornerRadius: 18)
                }
                .buttonStyle(.plain)
                .disabled(seconds <= 0)
                #if os(tvOS)
                .focusEffectDisabled()
                #endif

                Button { onChange(min(60, seconds + 1)) } label: {
                    Image(systemName: "plus")
                        .font(.system(size: 16, weight: .semibold))
                        .foregroundColor(Tokens.fg)
                        .frame(width: 36, height: 36)
                        .background(Tokens.bgCard)
                        .clipShape(Circle())
                        .cinematicFocusFollower(cornerRadius: 18)
                }
                .buttonStyle(.plain)
                .disabled(seconds >= 60)
                #if os(tvOS)
                .focusEffectDisabled()
                #endif
            }
        }
        .padding(.horizontal, compact ? Space.s3 : Space.s4)
        .frame(minHeight: compact ? 48 : 60)
        .background(Tokens.bgSoft)
        .clipShape(RoundedRectangle(cornerRadius: Radius.row, style: .continuous))
        // No outer cinematicFocus — the +/− buttons inside are the
        // focus targets. Wrapping the row in another focusable starved
        // the inner buttons of focus on tvOS.
    }
}

/// Numeric stepper for the LIVE preview-row decode budget. 0 = preview
/// video off (every tile shows its thumbnail). Otherwise the value is
/// the number of simultaneous decodes allowed; tiles past this number
/// fall back to the static thumbnail. Capped at the device's hardware
/// max (`DecodeBudget.hardwareCap`). Same +/− shape as `LiveOffsetRow`.
private struct PreviewVideoSlotsRow: View {
    let slots: Int
    let hardwareCap: Int
    let compact: Bool
    let onChange: (Int) -> Void

    var body: some View {
        HStack {
            VStack(alignment: .leading, spacing: 2) {
                Text("Preview video (live tiles on Home)")
                    .font(compact ? AppType.body(size: 15) : AppType.body())
                    .foregroundColor(Tokens.fg)
                    .lineLimit(1)
                Text(subtitle)
                    .font(AppType.monoSm())
                    .foregroundColor(Tokens.fgFaint)
            }
            Spacer()
            HStack(spacing: Space.s2) {
                Button { onChange(slots - 1) } label: {
                    Image(systemName: "minus")
                        .font(.system(size: 16, weight: .semibold))
                        .foregroundColor(Tokens.fg)
                        .frame(width: 36, height: 36)
                        .background(Tokens.bgCard)
                        .clipShape(Circle())
                        .cinematicFocusFollower(cornerRadius: 18)
                }
                .buttonStyle(.plain)
                .disabled(slots <= 0)
                #if os(tvOS)
                .focusEffectDisabled()
                #endif

                Button { onChange(slots + 1) } label: {
                    Image(systemName: "plus")
                        .font(.system(size: 16, weight: .semibold))
                        .foregroundColor(Tokens.fg)
                        .frame(width: 36, height: 36)
                        .background(Tokens.bgCard)
                        .clipShape(Circle())
                        .cinematicFocusFollower(cornerRadius: 18)
                }
                .buttonStyle(.plain)
                .disabled(slots >= hardwareCap)
                #if os(tvOS)
                .focusEffectDisabled()
                #endif
            }
        }
        .padding(.horizontal, compact ? Space.s3 : Space.s4)
        .frame(minHeight: compact ? 48 : 60)
        .background(Tokens.bgSoft)
        .clipShape(RoundedRectangle(cornerRadius: Radius.row, style: .continuous))
    }

    private var subtitle: String {
        if slots <= 0 { return "Off — thumbnails only" }
        return "\(slots) of \(hardwareCap) (device max) decoding"
    }
}

/// Toggle row — same pattern as `SettingRow` (focusable HStack + tap
/// gesture), with a custom switch graphic on the trailing edge so we
/// don't nest a focusable SwiftUI `Toggle` inside the cinematic-focus
/// wrapper. Pressing Select fires `onChange(!isOn)`.
///
/// Wrapping the row in a SwiftUI Button + `.buttonStyle(.plain)` +
/// `.focusEffectDisabled()` produced a visible white "card" highlight
/// on tvOS — the system focus effect leaked through despite
/// `focusEffectDisabled()`. The HStack-with-`.cinematicFocus` shape
/// matches `SettingRow` exactly and avoids that whole class of bug.
private struct ToggleRow: View {
    let label: String
    let isOn: Bool
    let compact: Bool
    let onChange: (Bool) -> Void

    var body: some View {
        HStack {
            Text(label)
                .font(compact ? AppType.body(size: 15) : AppType.body())
                .foregroundColor(Tokens.fg)
                .lineLimit(2)
            Spacer()
            switchGraphic
        }
        .padding(.horizontal, compact ? Space.s3 : Space.s4)
        .frame(minHeight: compact ? 38 : 48)
        .background(Tokens.bgSoft)
        .clipShape(RoundedRectangle(cornerRadius: Radius.row, style: .continuous))
        .cinematicFocus(cornerRadius: Radius.row)
        .contentShape(Rectangle())
        .onTapGesture { onChange(!isOn) }
    }

    private var switchGraphic: some View {
        let trackW: CGFloat = compact ? 42 : 50
        let trackH: CGFloat = compact ? 26 : 30
        let thumb: CGFloat = trackH - 4
        let travel: CGFloat = (trackW - thumb) / 2 - 2
        return ZStack {
            RoundedRectangle(cornerRadius: trackH / 2, style: .continuous)
                .fill(isOn ? Tokens.accent : Tokens.bgCard)
                .frame(width: trackW, height: trackH)
            Circle()
                .fill(Color.white)
                .frame(width: thumb, height: thumb)
                .offset(x: isOn ? travel : -travel)
        }
        .animation(.easeOut(duration: Motion.focusS), value: isOn)
    }
}

/// Destructive action row — used at the bottom of the Advanced picker
/// for "Reset All Settings". Same shape as `SettingRow` / `ToggleRow`
/// (focusable HStack + tap gesture, cinematic focus ring) but the
/// label renders in `Tokens.destructive` red so users see the danger
/// signal before they tap. Tap fires `onTap`; the caller is expected
/// to surface a confirmation alert before doing anything irreversible.
private struct DestructiveRow: View {
    let label: String
    let compact: Bool
    let onTap: () -> Void

    var body: some View {
        HStack {
            Text(label)
                .font(compact ? AppType.body(size: 15) : AppType.body())
                .foregroundColor(Tokens.destructive)
            Spacer()
        }
        .padding(.horizontal, compact ? Space.s3 : Space.s4)
        .frame(height: compact ? 42 : 56)
        .background(Tokens.bgSoft)
        .clipShape(RoundedRectangle(cornerRadius: Radius.row, style: .continuous))
        .cinematicFocus(cornerRadius: Radius.row)
        .contentShape(Rectangle())
        .onTapGesture(perform: onTap)
    }
}
