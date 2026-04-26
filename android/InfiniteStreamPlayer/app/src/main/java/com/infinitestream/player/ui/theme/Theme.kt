@file:kotlin.OptIn(androidx.tv.material3.ExperimentalTvMaterial3Api::class)

package com.infinitestream.player.ui.theme

import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.tv.material3.LocalContentColor
import androidx.tv.material3.MaterialTheme
import androidx.tv.material3.darkColorScheme

@Composable
fun InfiniteStreamTheme(content: @Composable () -> Unit) {
    val colors = darkColorScheme(
        primary    = Tokens.accent,
        secondary  = Tokens.accent,
        background = Tokens.bg,
        surface    = Tokens.bgSoft,
        onBackground = Tokens.fg,
        onSurface  = Tokens.fg,
    )
    MaterialTheme(colorScheme = colors) {
        CompositionLocalProvider(LocalContentColor provides Tokens.fg) {
            content()
        }
    }
}
