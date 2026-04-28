package com.infinitestream.player.ui.component

import androidx.compose.animation.core.RepeatMode
import androidx.compose.animation.core.animateFloat
import androidx.compose.animation.core.infiniteRepeatable
import androidx.compose.animation.core.rememberInfiniteTransition
import androidx.compose.animation.core.tween
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp

/** A small dot that breathes — used at the bottom of the server picker
 *  while discovery is active. Spec: "pulse a small dot at the bottom of
 *  the screen when scanning." */
@Composable
fun PulseDot(color: Color, modifier: Modifier = Modifier, size: Dp = 10.dp) {
    val transition = rememberInfiniteTransition(label = "pulseDot")
    val alpha by transition.animateFloat(
        initialValue = 0.3f,
        targetValue = 1f,
        animationSpec = infiniteRepeatable(
            animation = tween(durationMillis = 900),
            repeatMode = RepeatMode.Reverse,
        ),
        label = "pulseDot.alpha",
    )
    Box(
        modifier = modifier
            .size(size)
            .alpha(alpha)
            .clip(CircleShape)
            .background(color)
    )
}
