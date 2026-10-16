package com.nervus.sysui

import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.State
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.withContext

/**
 * The result of one polled fetch.
 *
 * "Loading", "has data" and "failed" are deliberately three fields side by side rather than three branches of a sealed class:
 * while refreshing, the old data should stay on screen (or the list would blank every 3 seconds), and the same
 * goes for errors: one network hiccup shouldn't clear a list that is already showing.
 */
data class Polled<T>(
    val value: T,
    val loading: Boolean = false,
    val error: String? = null,
)

/**
 * Calls [fetch] every [intervalMs] and exposes the result as Compose state.
 *
 * ## Why polling and not a subscription
 *
 * nervud [does not implement] Subscribe yet: handleReady in `ipc/conn.go` treats
 * Subscribe / Unsubscribe / Cancel as unsupported and **closes the connection** when it sees them.
 * `NervusApp.subscribe()` and SubscriptionManager exist in the SDK, but calling them
 * makes the connection drop for no visible reason, with the symptom far from the cause.
 *
 * So the system UI always polls. Once the kernel supports subscriptions, swap this file for an event stream;
 * callers (who just hold a State) won't change.
 *
 * [fetch] runs on an IO thread, so it never blocks rendering.
 */
/**
 * @param key External state the fetch depends on. When it changes, the polling loop restarts [immediately].
 *
 * Without it there's a subtle bug: `LaunchedEffect` only restarts when its key changes, while
 * [fetch] is a closure rebuilt on every recomposition, so the loop keeps running the [first] closure and
 * the external state it captured back then. It shows up as "I changed folder and the list didn't move", looks like
 * a caching problem, and nobody thinks of the closure. If fetch reads any outside variable, pass it in key.
 */
@Composable
fun <T> rememberPolled(
    initial: T,
    intervalMs: Long = 3000,
    key: Any? = Unit,
    fetch: suspend () -> T,
): State<Polled<T>> {
    val state = remember { mutableStateOf(Polled(initial, loading = true)) }

    LaunchedEffect(intervalMs, key) {
        while (isActive) {
            state.value = state.value.copy(loading = true)
            try {
                val fresh = withContext(Dispatchers.IO) { fetch() }
                state.value = Polled(fresh, loading = false, error = null)
            } catch (e: Exception) {
                // Keep the previous value: a failed refresh shouldn't make content that is already shown disappear
                state.value = state.value.copy(
                    loading = false,
                    error = e.message ?: e::class.simpleName ?: "unknown error",
                )
            }
            delay(intervalMs)
        }
    }

    return state
}
