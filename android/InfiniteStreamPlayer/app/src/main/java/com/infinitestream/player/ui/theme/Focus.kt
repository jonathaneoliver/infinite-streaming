package com.infinitestream.player.ui.theme

import androidx.compose.animation.core.animateDpAsState
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.tween
import androidx.compose.foundation.border
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.composed
import androidx.compose.ui.draw.scale
import androidx.compose.ui.draw.shadow
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp

/**
 * The single source of truth for "this thing is focused" — every interactive
 * element in the app should hang `.tvFocus(...)` off its outermost modifier.
 *
 * Spec (HANDOFF.md): 3 px white outline + 1.04× scale + 18 px drop-shadow,
 * 180 ms ease-out. Designed to be unmistakable from across the room — the #1
 * usability complaint on the previous UI.
 */
fun Modifier.tvFocus(
    cornerRadius: Dp = 12.dp,
    scaleFocused: Float = 1.04f,
    ringColor: Color = Color.White,
    ringWidth: Dp = 3.dp,
    elevation: Dp = 18.dp,
): Modifier = composed {
    var focused by remember { mutableStateOf(false) }
    val scale by animateFloatAsState(
        targetValue = if (focused) scaleFocused else 1f,
        animationSpec = tween(durationMillis = Motion.focusMs),
        label = "tvFocus.scale",
    )
    val shadow by animateDpAsState(
        targetValue = if (focused) elevation else 0.dp,
        animationSpec = tween(durationMillis = Motion.focusMs),
        label = "tvFocus.shadow",
    )
    val ring by animateDpAsState(
        targetValue = if (focused) ringWidth else 0.dp,
        animationSpec = tween(durationMillis = Motion.focusMs),
        label = "tvFocus.ring",
    )
    val shape = RoundedCornerShape(cornerRadius)
    this
        .onFocusChanged { focused = it.isFocused }
        .scale(scale)
        .shadow(elevation = shadow, shape = shape, clip = false)
        .border(width = ring, color = ringColor, shape = shape)
}
