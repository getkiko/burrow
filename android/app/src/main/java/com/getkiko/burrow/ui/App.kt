package com.getkiko.burrow.ui

import androidx.activity.compose.BackHandler
import androidx.compose.animation.AnimatedContent
import androidx.compose.animation.core.tween
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.slideInHorizontally
import androidx.compose.animation.slideOutHorizontally
import androidx.compose.animation.togetherWith
import androidx.compose.foundation.layout.padding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Article
import androidx.compose.material.icons.filled.BugReport
import androidx.compose.material.icons.filled.Hub
import androidx.compose.material3.Icon
import androidx.compose.material3.NavigationBar
import androidx.compose.material3.NavigationBarItem
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import com.getkiko.burrow.ConfigStore
import com.getkiko.burrow.TunnelService

/** The three bottom tabs. */
private enum class Tab(val label: String) {
    TUNNEL("Tunnel"),
    LOG("Logs"),
    DIAG("Debug"),
}

@Composable
fun App(service: TunnelService?, configStore: ConfigStore) {
    var selected by rememberSaveable { mutableIntStateOf(0) }
    var showConfig by rememberSaveable { mutableStateOf(false) }

    // System back: while the config screen is open, go back to the main screen
    // instead of letting the Activity finish (which used to quit the app and
    // make users think their config was lost). enabled = showConfig leaves back
    // alone on the main screen so it still exits the app.
    BackHandler(enabled = showConfig) { showConfig = false }

    // AnimatedContent handles the main <-> config transition, using the
    // standard Android secondary-screen motion: on enter the new screen slides
    // in from the right while the old one moves left, reversed on return, with
    // a cross-fade.
    AnimatedContent(
        targetState = showConfig,
        transitionSpec = {
            val duration = 300
            if (targetState) {
                // Enter config: slide in from the right, main screen exits left.
                (slideInHorizontally(tween(duration)) { it } + fadeIn(tween(duration)))
                    .togetherWith(
                        slideOutHorizontally(tween(duration)) { -it / 4 } + fadeOut(tween(duration)),
                    )
            } else {
                // Back to main: main slides back from the left, config exits right.
                (slideInHorizontally(tween(duration)) { -it / 4 } + fadeIn(tween(duration)))
                    .togetherWith(
                        slideOutHorizontally(tween(duration)) { it } + fadeOut(tween(duration)),
                    )
            }
        },
        label = "screen transition",
    ) { isConfig ->
        if (isConfig) {
            ConfigScreen(configStore = configStore, onBack = { showConfig = false })
        } else {
            MainTabs(
                service = service,
                configStore = configStore,
                selected = selected,
                onSelect = { selected = it },
                onOpenConfig = { showConfig = true },
            )
        }
    }
}

/** The three-tab scaffold, split out of App() so AnimatedContent can transition it as a whole. */
@Composable
private fun MainTabs(
    service: TunnelService?,
    configStore: ConfigStore,
    selected: Int,
    onSelect: (Int) -> Unit,
    onOpenConfig: () -> Unit,
) {
    Scaffold(
        bottomBar = {
            NavigationBar {
                Tab.entries.forEachIndexed { index, tab ->
                    NavigationBarItem(
                        selected = selected == index,
                        onClick = { onSelect(index) },
                        icon = {
                            Icon(
                                imageVector = when (tab) {
                                    Tab.TUNNEL -> Icons.Filled.Hub
                                    Tab.LOG -> Icons.AutoMirrored.Filled.Article
                                    Tab.DIAG -> Icons.Filled.BugReport
                                },
                                contentDescription = tab.label,
                            )
                        },
                        label = { Text(tab.label) },
                    )
                }
            }
        },
    ) { padding ->
        val modifier = Modifier.padding(padding)
        when (Tab.entries[selected]) {
            Tab.TUNNEL -> TunnelScreen(
                service = service,
                configStore = configStore,
                onOpenConfig = onOpenConfig,
                modifier = modifier,
            )
            Tab.LOG -> LogScreen(service = service, modifier = modifier)
            Tab.DIAG -> DiagScreen(configStore = configStore, modifier = modifier)
        }
    }
}
