package com.infinitestream.player.ui.theme

import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.googlefonts.Font
import androidx.compose.ui.text.googlefonts.GoogleFont
import androidx.compose.ui.unit.sp
import com.infinitestream.player.R

/**
 * Three-family ramp from HANDOFF.md:
 *   - Fraunces (display)        — hero titles, headers, big numerals
 *   - Inter Tight (UI sans)     — body, buttons, labels
 *   - JetBrains Mono            — timecodes, hostnames, metadata pills
 *
 * Loaded via Google's downloadable-fonts provider so we don't ship 1MB+ of
 * TTFs in the APK. First-launch fallback resolves to system serif/sans/mono.
 */

private val provider = GoogleFont.Provider(
    providerAuthority = "com.google.android.gms.fonts",
    providerPackage = "com.google.android.gms",
    certificates = R.array.com_google_android_gms_fonts_certs
)

private val Fraunces = FontFamily(
    Font(googleFont = GoogleFont("Fraunces"), fontProvider = provider, weight = FontWeight.Normal),
    Font(googleFont = GoogleFont("Fraunces"), fontProvider = provider, weight = FontWeight.Medium),
)

private val InterTight = FontFamily(
    Font(googleFont = GoogleFont("Inter Tight"), fontProvider = provider, weight = FontWeight.Normal),
    Font(googleFont = GoogleFont("Inter Tight"), fontProvider = provider, weight = FontWeight.Medium),
    Font(googleFont = GoogleFont("Inter Tight"), fontProvider = provider, weight = FontWeight.SemiBold),
)

private val JBMono = FontFamily(
    Font(googleFont = GoogleFont("JetBrains Mono"), fontProvider = provider, weight = FontWeight.Normal),
    Font(googleFont = GoogleFont("JetBrains Mono"), fontProvider = provider, weight = FontWeight.Medium),
)

object AppType {
    val display    = TextStyle(fontFamily = Fraunces,   fontWeight = FontWeight.Medium, fontSize = 30.sp)
    val displayLg  = TextStyle(fontFamily = Fraunces,   fontWeight = FontWeight.Medium, fontSize = 44.sp)
    val title      = TextStyle(fontFamily = Fraunces,   fontWeight = FontWeight.Medium, fontSize = 22.sp)
    val titleSm    = TextStyle(fontFamily = Fraunces,   fontWeight = FontWeight.Normal, fontSize = 18.sp)

    val body       = TextStyle(fontFamily = InterTight, fontWeight = FontWeight.Normal,   fontSize = 14.sp)
    val bodySm     = TextStyle(fontFamily = InterTight, fontWeight = FontWeight.Normal,   fontSize = 12.sp)
    val label      = TextStyle(fontFamily = InterTight, fontWeight = FontWeight.SemiBold, fontSize = 12.sp)
    val button     = TextStyle(fontFamily = InterTight, fontWeight = FontWeight.SemiBold, fontSize = 14.sp)

    val mono       = TextStyle(fontFamily = JBMono,     fontWeight = FontWeight.Normal,   fontSize = 12.sp)
    val monoSm     = TextStyle(fontFamily = JBMono,     fontWeight = FontWeight.Normal,   fontSize = 10.sp)
    val monoLg     = TextStyle(fontFamily = JBMono,     fontWeight = FontWeight.Medium,   fontSize = 14.sp)
}
