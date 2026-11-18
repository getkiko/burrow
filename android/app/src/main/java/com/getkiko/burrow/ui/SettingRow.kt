package com.getkiko.burrow.ui

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.KeyboardArrowRight
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ExposedDropdownMenuBox
import androidx.compose.material3.ExposedDropdownMenuDefaults
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.MenuAnchorType
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TextField
import androidx.compose.material3.TextFieldColors
import androidx.compose.material3.TextFieldDefaults
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateMapOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.text.input.KeyboardType

/**
 * Converts full-width characters to half-width.
 *
 * Full-width punctuation is the easiest mistake with a CJK input method: a
 * full-width colon (U+FF1A) in an address such as "127.0.0.1:8080" looks
 * almost identical to ':' but breaks address parsing. The full-width block is contiguous in Unicode
 * (U+FF01..U+FF5E) and sits a fixed 0xFEE0 above its ASCII counterpart; the
 * ideographic space U+3000 is outside that block and handled separately.
 *
 * Converting instead of rejecting: the intent is clear (the user wanted a
 * colon), and rejecting would just feel like the key did nothing.
 */
internal fun String.toHalfWidth(): String = map { c ->
    when (c.code) {
        0x3000 -> ' '
        in 0xFF01..0xFF5E -> (c.code - 0xFEE0).toChar()
        else -> c
    }
}.joinToString("")

/**
 * Shared text field colors for the whole app.
 *
 * Material 3 filled TextFields default to a grey surfaceVariant container,
 * and a dozen of them stack up into a dull grey block. Instead:
 * - the container is fully transparent, with a thin bottom line marking the field
 * - unfocused, the line is a light outlineVariant and stays in the background
 * - focused, the line takes the primary color and gets thicker
 *
 * The result is a line of text over an underline, much lighter than grey
 * boxes and closer to native input.
 */
@Composable
fun appTextFieldColors(): TextFieldColors = TextFieldDefaults.colors(
    focusedContainerColor = Color.Transparent,
    unfocusedContainerColor = Color.Transparent,
    disabledContainerColor = Color.Transparent,
    errorContainerColor = Color.Transparent,
    focusedIndicatorColor = MaterialTheme.colorScheme.primary,
    unfocusedIndicatorColor = MaterialTheme.colorScheme.outlineVariant,
    disabledIndicatorColor = MaterialTheme.colorScheme.outlineVariant.copy(alpha = 0.5f),
)

/**
 * Shared setting row: label on the left, current value on the right, whole
 * row clickable.
 *
 * This is the basic unit of the config screen. One text field per setting
 * would stack a dozen outlined boxes into visual noise, for settings most
 * users never change. So each setting is a quiet one-line row (label plus
 * current value), and tapping it opens a dialog to edit that single field.
 *
 * @param label       setting name on the left
 * @param value       current value on the right; empty shows the placeholder
 * @param placeholder grey hint shown when value is empty
 * @param enabled     when false the row is greyed out and not clickable
 */
@Composable
fun SettingRow(
    label: String,
    value: String,
    modifier: Modifier = Modifier,
    placeholder: String = "Not set",
    enabled: Boolean = true,
    onClick: () -> Unit,
) {
    val labelColor = if (enabled) {
        MaterialTheme.colorScheme.onSurface
    } else {
        MaterialTheme.colorScheme.onSurface.copy(alpha = 0.38f)
    }
    val valueColor = when {
        !enabled -> MaterialTheme.colorScheme.onSurface.copy(alpha = 0.38f)
        value.isBlank() -> MaterialTheme.colorScheme.onSurfaceVariant.copy(alpha = 0.6f)
        else -> MaterialTheme.colorScheme.primary
    }
    Row(
        modifier = modifier
            .fillMaxWidth()
            .clickable(enabled = enabled, onClick = onClick)
            .padding(vertical = 14.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(
            text = label,
            style = MaterialTheme.typography.bodyLarge,
            color = labelColor,
        )
        Spacer(Modifier.weight(1f))
        Spacer(Modifier.width(12.dp))
        Text(
            text = value.ifBlank { placeholder },
            style = MaterialTheme.typography.bodyMedium,
            color = valueColor,
            maxLines = 1,
            overflow = TextOverflow.Ellipsis,
            modifier = Modifier.weight(1.4f, fill = false),
        )
    }
}

/** A setting row with a switch operated directly on the row, no dialog. */
@Composable
fun SwitchRow(
    label: String,
    checked: Boolean,
    modifier: Modifier = Modifier,
    description: String? = null,
    onCheckedChange: (Boolean) -> Unit,
) {
    Row(
        modifier = modifier
            .fillMaxWidth()
            .padding(vertical = 6.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(modifier = Modifier.weight(1f)) {
            Text(label, style = MaterialTheme.typography.bodyLarge)
            if (description != null) {
                Text(
                    description,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        Spacer(Modifier.width(12.dp))
        Switch(checked = checked, onCheckedChange = onCheckedChange)
    }
}

/**
 * Single-field edit dialog: one text field plus Cancel/OK.
 *
 * The text being edited lives in the dialog's own state and is written back
 * only on OK; Cancel or tapping outside leaves the value unchanged, as users
 * expect from a dialog.
 */
@Composable
fun TextEditDialog(
    title: String,
    initial: String,
    onDismiss: () -> Unit,
    onConfirm: (String) -> Unit,
    hint: String? = null,
    keyboardType: KeyboardType = KeyboardType.Text,
    sanitize: (String) -> String = { it },
    validate: (String) -> String? = { null },
) {
    var text by remember { mutableStateOf(initial) }
    // Validate as the user types: a bad value is flagged in red with the reason
    // right away, rather than as an error at the bottom of the page after the
    // dialog has closed and the user has to remember which field was wrong.
    // An empty string is not an error (it is unfinished, not wrong), but OK
    // stays disabled so an empty value is never written back.
    val errorText = if (text.isBlank()) null else validate(text)
    val canConfirm = text.isNotBlank() && errorText == null
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(title) },
        text = {
            Column {
                TextField(
                    colors = appTextFieldColors(),
                    value = text,
                    onValueChange = { text = sanitize(it) },
                    singleLine = true,
                    isError = errorText != null,
                    keyboardOptions = KeyboardOptions(keyboardType = keyboardType),
                    modifier = Modifier.fillMaxWidth(),
                )
                Spacer(Modifier.height(8.dp))
                // Show the error (error color) if there is one, otherwise the hint
                // (secondary color). Both use the same slot, so an error does not push
                // the content below it down.
                if (errorText != null) {
                    Text(
                        errorText,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.error,
                    )
                } else if (hint != null) {
                    Text(
                        hint,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
        },
        confirmButton = {
            TextButton(
                onClick = { onConfirm(text); onDismiss() },
                enabled = canConfirm,
            ) { Text("OK") }
        },
        dismissButton = {
            TextButton(onClick = onDismiss) { Text("Cancel") }
        },
    )
}

/**
 * Single-choice dialog: an enum setting lists all of its values.
 *
 * A free-text field for an enum is easy to get wrong and only fails at Go
 * validation; listing the valid values lets the user just pick one.
 */
@Composable
fun SingleChoiceDialog(
    title: String,
    options: List<String>,
    selected: String,
    onDismiss: () -> Unit,
    onSelect: (String) -> Unit,
    descriptions: Map<String, String> = emptyMap(),
) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(title) },
        text = {
            Column {
                options.forEach { option ->
                    Row(
                        modifier = Modifier
                            .fillMaxWidth()
                            .clickable { onSelect(option); onDismiss() }
                            .padding(vertical = 12.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        androidx.compose.material3.RadioButton(
                            selected = option == selected,
                            onClick = { onSelect(option); onDismiss() },
                        )
                        Spacer(Modifier.width(8.dp))
                        Column {
                            Text(option, style = MaterialTheme.typography.bodyLarge)
                            descriptions[option]?.let {
                                Text(
                                    it,
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                )
                            }
                        }
                    }
                }
            }
        },
        confirmButton = {},
        dismissButton = {
            TextButton(onClick = onDismiss) { Text("Cancel") }
        },
    )
}

/**
 * Interface picker: choose from the interfaces that actually exist on this
 * device; no manual entry.
 *
 * An interface name is not something users can make up. Typing a name that
 * does not exist only makes punching fail, and the error shows up only after
 * starting. The list comes from the system's actual interfaces, so a choice
 * from it is always valid.
 *
 * (The Go side looks interfaces up by name via ioctl to avoid Android's
 * netlink restrictions; that is at tunnel runtime. The app enumerates
 * interfaces through a different path, so the list is available here.)
 */
@Composable
fun InterfaceDialog(
    initial: String,
    candidates: List<String>,
    onDismiss: () -> Unit,
    onConfirm: (String) -> Unit,
) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Network interface") },
        text = {
            if (candidates.isEmpty()) {
                Text(
                    "No usable network interface found.\n\n" +
                        "Connect to Wi-Fi or mobile data, then reopen this screen.",
                    style = MaterialTheme.typography.bodyMedium,
                )
            } else {
                Column(modifier = Modifier.verticalScroll(rememberScrollState())) {
                    candidates.forEach { name ->
                        Row(
                            modifier = Modifier
                                .fillMaxWidth()
                                .clickable { onConfirm(name); onDismiss() }
                                .padding(vertical = 12.dp),
                            verticalAlignment = Alignment.CenterVertically,
                        ) {
                            androidx.compose.material3.RadioButton(
                                selected = name == initial,
                                onClick = { onConfirm(name); onDismiss() },
                            )
                            Spacer(Modifier.width(8.dp))
                            Text(name, style = MaterialTheme.typography.bodyLarge)
                        }
                    }
                    Spacer(Modifier.height(8.dp))
                    Text(
                        "Required. With 0.0.0.0 the kernel may pick an unroutable source address and punching will fail",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
        },
        confirmButton = {},
        dismissButton = {
            TextButton(onClick = onDismiss) { Text("Cancel") }
        },
    )
}

/** Section title: hierarchy through spacing and font weight, not a Card. */
@Composable
fun SectionHeader(title: String, modifier: Modifier = Modifier) {
    Text(
        text = title,
        style = MaterialTheme.typography.labelLarge,
        color = MaterialTheme.colorScheme.primary,
        modifier = modifier.padding(top = 24.dp, bottom = 4.dp),
    )
}

/**
 * Section container: a very light surfaceContainer background with rounded
 * corners groups a set of rows.
 *
 * There is no outline, only a barely visible background difference that
 * gives adjacent sections a natural boundary and the page a card-like rhythm.
 * Rows inside a section are separated by 1dp dividers indented to the text
 * start (not full width), the standard iOS/Material settings layout, so rows
 * read as a group rather than floating separately.
 */
@Composable
fun SettingGroup(
    modifier: Modifier = Modifier,
    content: @Composable ColumnScope.() -> Unit,
) {
    Surface(
        modifier = modifier.fillMaxWidth(),
        shape = RoundedCornerShape(16.dp),
        color = MaterialTheme.colorScheme.surfaceContainerLow,
    ) {
        Column(modifier = Modifier.padding(horizontal = 16.dp, vertical = 4.dp), content = content)
    }
}

/** Divider between rows in a section: very light and not full width. */
@Composable
fun RowDivider() {
    HorizontalDivider(
        color = MaterialTheme.colorScheme.outlineVariant.copy(alpha = 0.4f),
        thickness = 1.dp,
    )
}

/**
 * Summary row: a title with a grey plain-text summary below; the whole row is
 * clickable.
 *
 * Used as the entry point for whole parameter blocks such as birthday /
 * predictive. Laid out as a pile of text fields they are too noisy, so the
 * row shows a one-line summary (e.g. "256 candidates • ports 1024-65535 •
 * timeout 5s") and tapping it edits all parameters in a dialog. The summary
 * must be computed from the current values, not hard-coded, or edits would
 * show no feedback.
 */
@Composable
fun SummaryRow(
    title: String,
    summary: String,
    modifier: Modifier = Modifier,
    onClick: () -> Unit,
) {
    Row(
        modifier = modifier
            .fillMaxWidth()
            .clickable(onClick = onClick)
            .padding(vertical = 14.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(modifier = Modifier.weight(1f)) {
            Text(title, style = MaterialTheme.typography.bodyLarge)
            Spacer(Modifier.height(2.dp))
            Text(
                summary,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 2,
                overflow = TextOverflow.Ellipsis,
            )
        }
        Spacer(Modifier.width(8.dp))
        Icon(
            imageVector = Icons.AutoMirrored.Filled.KeyboardArrowRight,
            contentDescription = null,
            tint = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

/** Describes one field in a multi-field dialog. [isInt] selects the keyboard type and how the value is parsed on write-back. */
data class DialogField(
    val key: String,
    val label: String,
    val value: String,
    val isInt: Boolean = false,
    /** When non-null, the field renders as a single-choice dropdown instead of a text field. */
    val options: List<String>? = null,
)

/**
 * Multi-field edit dialog: edits a whole birthday / predictive block at once.
 *
 * All fields being edited live in the dialog's own map and are written back
 * together only on OK, the same transaction semantics as TextEditDialog:
 * Cancel changes nothing. The content can exceed one screen (birthday has 8
 * fields), so it scrolls vertically; otherwise OK could be pushed off screen
 * on small devices.
 */
@Composable
fun MultiFieldDialog(
    title: String,
    fields: List<DialogField>,
    onDismiss: () -> Unit,
    onConfirm: (Map<String, String>) -> Unit,
) {
    val edited = remember { mutableStateMapOf<String, String>().apply {
        fields.forEach { put(it.key, it.value) }
    } }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(title) },
        text = {
            Column(
                modifier = Modifier.verticalScroll(rememberScrollState()),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                fields.forEach { field ->
                    val current = edited[field.key] ?: ""
                    if (field.options != null) {
                        DialogDropdown(
                            label = field.label,
                            value = current,
                            options = field.options,
                            onSelect = { edited[field.key] = it },
                        )
                    } else {
                        TextField(
                            colors = appTextFieldColors(),
                            value = current,
                            // Numeric fields accept digits only: a Number keyboard does not stop
                            // paste, hardware keyboards or some IMEs, so filter here. Non-numeric
                            // fields (durations like "250ms") only get full-width to half-width
                            // conversion.
                            onValueChange = { input ->
                                edited[field.key] = if (field.isInt) {
                                    input.filter { it.isDigit() }
                                } else {
                                    input.toHalfWidth()
                                }
                            },
                            label = { Text(field.label) },
                            singleLine = true,
                            keyboardOptions = KeyboardOptions(
                                keyboardType = if (field.isInt) {
                                    KeyboardType.Number
                                } else {
                                    KeyboardType.Text
                                },
                            ),
                            modifier = Modifier.fillMaxWidth(),
                        )
                    }
                }
            }
        },
        confirmButton = {
            TextButton(onClick = { onConfirm(edited.toMap()); onDismiss() }) { Text("OK") }
        },
        dismissButton = {
            TextButton(onClick = onDismiss) { Text("Cancel") }
        },
    )
}

/** Enum dropdown inside a dialog (e.g. ack/fanout for birthday.selection). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun DialogDropdown(
    label: String,
    value: String,
    options: List<String>,
    onSelect: (String) -> Unit,
) {
    var expanded by remember { mutableStateOf(false) }
    ExposedDropdownMenuBox(
        expanded = expanded,
        onExpandedChange = { expanded = it },
    ) {
        TextField(
            colors = appTextFieldColors(),
            value = value,
            onValueChange = {},
            readOnly = true,
            label = { Text(label) },
            trailingIcon = { ExposedDropdownMenuDefaults.TrailingIcon(expanded = expanded) },
            modifier = Modifier
                .menuAnchor(MenuAnchorType.PrimaryNotEditable)
                .fillMaxWidth(),
        )
        ExposedDropdownMenu(expanded = expanded, onDismissRequest = { expanded = false }) {
            options.forEach { option ->
                DropdownMenuItem(
                    text = { Text(option) },
                    onClick = { onSelect(option); expanded = false },
                )
            }
        }
    }
}
