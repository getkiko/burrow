package com.getkiko.burrow.ui

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.dp
import com.getkiko.burrow.TunnelService

@Composable
fun LogScreen(service: TunnelService?, modifier: Modifier = Modifier) {
    if (service == null) {
        Column(modifier = modifier.padding(16.dp)) { Text("Connecting to service...") }
        return
    }

    val logs by service.logs.collectAsState()
    val listState = rememberLazyListState()

    // logs.size cannot be the key: the service's log buffer holds 1000 lines
    // and, once full, drops the oldest as it appends, so the size stays constant
    // and auto-scroll would stop working. Key on the last line's seq instead,
    // which increases monotonically whether or not the buffer is full.
    LaunchedEffect(logs.lastOrNull()?.seq) {
        if (logs.isNotEmpty()) {
            listState.animateScrollToItem(logs.size - 1)
        }
    }

    Column(modifier = modifier.fillMaxSize()) {
        TextButton(
            onClick = { service.clearLogs() },
            modifier = Modifier.padding(horizontal = 8.dp),
        ) { Text("Clear (${logs.size})") }

        LazyColumn(
            state = listState,
            modifier = Modifier.fillMaxWidth().padding(horizontal = 8.dp),
        ) {
            // Key by seq rather than the implicit index: once capped at 1000 lines,
            // the same index maps to a constantly changing line, which breaks
            // LazyColumn item reuse. seq is a monotonically increasing number assigned
            // by the service, unique and stable per line.
            items(logs, key = { it.seq }) { line ->
                Text(
                    text = line.text,
                    color = levelColor(line.level),
                    style = MaterialTheme.typography.bodySmall,
                    fontFamily = FontFamily.Monospace,
                )
            }
        }
    }
}

/** Colors a line by log level: warn orange, error red, everything else default. */
@Composable
private fun levelColor(level: String): Color = when (level) {
    "error" -> MaterialTheme.colorScheme.error
    "warn" -> Color(0xFFEF6C00)
    else -> MaterialTheme.colorScheme.onSurface
}
