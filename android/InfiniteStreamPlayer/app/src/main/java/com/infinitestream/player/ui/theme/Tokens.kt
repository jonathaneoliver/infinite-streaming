package com.infinitestream.player.ui.theme

import androidx.compose.runtime.Composable
import androidx.compose.runtime.ReadOnlyComposable
import androidx.compose.runtime.compositionLocalOf
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp

/**
 * Cinematic-rework design tokens. One source of truth for color, spacing,
 * radii, and motion. Values come from HANDOFF.md (Section "Design system").
 *
 * OKLCH values in the spec are pre-resolved to sRGB here so we don't pull in
 * a color-space library on a TV target where every hundred KB matters.
 */

// ---- Color ----------------------------------------------------------------

object Tokens {
    // Backgrounds — near-black neutrals tuned so OLED panels don't crush.
    val bg      = Color(0xFF06070A)
    val bgSoft  = Color(0xFF0E1014)
    val bgCard  = Color(0xFF14171C)

    // Foreground (warm bone, not pure white — kinder on TV at 10ft).
    val fg      = Color(0xFFF5F3EE)
    val fgDim   = Color(0x99F5F3EE) // 60%
    val fgFaint = Color(0x2EF5F3EE) // 18%
    val line    = Color(0x1AF5F3EE) // 10%

    // Accent: warm gold (oklch(0.78 0.13 80) ≈ #D6A957).
    val accent  = Color(0xFFD6A957)

    // LIVE badge: coral red (oklch(0.70 0.18 25) ≈ #EC5E4A).
    val live    = Color(0xFFEC5E4A)

    // Online status (green).
    val ok      = Color(0xFF4ADE80)
    val offline = Color(0xFF6B6F77)

    // Destructive red — used for irreversible actions ("Reset All
    // Settings"). Distinct from `live` so the playback LIVE badge and
    // a destructive setting don't read as the same affordance.
    val destructive = Color(0xFFEB4D4D)
}

// ---- Spacing scale --------------------------------------------------------

object Space {
    val s1: Dp  = 8.dp
    val s2: Dp  = 12.dp
    val s3: Dp  = 14.dp
    val s4: Dp  = 18.dp
    val s5: Dp  = 22.dp
    val s6: Dp  = 28.dp
    val s7: Dp  = 36.dp
    val s8: Dp  = 56.dp
}

// ---- Radii ----------------------------------------------------------------

object Radius {
    val card: Dp = 12.dp
    val cardLg: Dp = 14.dp
    val pill: Dp = 999.dp
    val row: Dp = 10.dp
}

// ---- Motion ---------------------------------------------------------------

object Motion {
    const val focusMs = 180
    const val drawerMs = 240
    const val hudFadeMs = 220
}
