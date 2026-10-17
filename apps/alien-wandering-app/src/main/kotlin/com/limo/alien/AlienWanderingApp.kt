package com.limo.alien

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.nervus.sdk.component.ComponentConfig
import com.nervus.sdk.component.InterfaceRequirement
import com.nervus.sdk.component.NervusApp
import com.nervus.sdk.ui.attachComposeDesktop
import io.github.nervusos.iface.basemotion.v1.SetVelocityRequest
import io.github.nervusos.iface.basemotion.v1.StopRequest
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import java.util.logging.Logger

private const val DRIVE_SPEED = 0.15   // m/s
private const val TURN_SPEED = 0.5     // rad/s

/**
 * NervusOS app (L1): the AgileX Limo Pro "Alien Wandering" demo, ported onto NervusOS.
 *
 * It no longer drives the Limo hardware or runs ROS. It holds a HUMAN control lease on the
 * base and streams **BaseMotion.setVelocity** at 20 Hz; the velocity comes from the on-screen
 * drive pad or the (placeholder) wander loop. NervusOS arbitrates and routes to the DimOS Limo
 * provider, which drives the base over serial. Target device: RDK S100 (linux-arm64).
 */
class AlienWanderingApp(
    config: ComponentConfig = ComponentConfig(
        componentId = "alien_wandering",
        socketPath = "/run/nervus/nervud.sock",
    ),
) : NervusApp(config) {

    override val requiredInterfaces = listOf(
        InterfaceRequirement(
            id = "nervus.interface.motion.base",
            minMajor = 1,
            maxMajor = 1,
            resourceType = "nervus.resource.motion.base",
            resourceRole = "main",
        )
    )

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    // Compose-observable command state (read by the control loop, written by the UI).
    private var vx by mutableStateOf(0.0)
    private var wz by mutableStateOf(0.0)
    private var wandering by mutableStateOf(false)

    override fun onReady() {
        val base = use(BaseMotion::class)
        // TODO(VERIFY): AcquireControl a HUMAN lease on nervus.resource.motion.base/main first.
        logger.info("alien_wandering ready — streaming BaseMotion.setVelocity + launching UI")
        scope.launch { controlLoop(base) }
        attachComposeDesktop(title = "Alien Wandering — Limo Pro", width = 460.dp, height = 380.dp) {
            AlienScreen()
        }
    }

    /** Stream the current velocity command to the base at 20 Hz. */
    private suspend fun controlLoop(base: BaseMotion) {
        while (scope.isActive) {
            if (wandering) {
                // Placeholder wander (slow creep). TODO: swap for the ported
                // WanderingMotionPolicy / VisualServoController / AckermannCommandLimiter,
                // fed by event-bus perception (scan + normalized visual target).
                vx = 0.10; wz = 0.0
            }
            try {
                base.setVelocity(
                    SetVelocityRequest.newBuilder().setLinearX(vx).setLinearY(0.0).setAngularZ(wz).build()
                )
            } catch (e: Exception) {
                logger.warning("setVelocity failed: ${e.message}")
            }
            delay(50)
        }
    }

    private fun drive(nvx: Double, nwz: Double) { wandering = false; vx = nvx; wz = nwz }

    private fun stopBase() {
        wandering = false; vx = 0.0; wz = 0.0
        scope.launch { runCatching { use(BaseMotion::class).stop(StopRequest.newBuilder().build()) } }
    }

    @OptIn(ExperimentalMaterial3Api::class)
    @Composable
    private fun AlienScreen() {
        MaterialTheme {
            Scaffold(topBar = { TopAppBar(title = { Text("Alien Wandering · Limo Pro") }) }) { pad ->
                Column(
                    modifier = Modifier.fillMaxSize().padding(pad).padding(16.dp),
                    verticalArrangement = Arrangement.spacedBy(12.dp),
                ) {
                    Card(modifier = Modifier.fillMaxWidth()) {
                        Text(
                            "BaseMotion@1 → NervusOS → DimOS → Limo\n" +
                                "cmd:  vx = ${"%.2f".format(vx)} m/s   ωz = ${"%.2f".format(wz)} rad/s",
                            modifier = Modifier.padding(12.dp),
                        )
                    }

                    FilterChip(
                        selected = wandering,
                        onClick = { wandering = !wandering },
                        label = { Text(if (wandering) "Wandering: ON (auto)" else "Wandering: OFF") },
                    )

                    // Drive pad
                    Row(horizontalArrangement = Arrangement.spacedBy(8.dp), modifier = Modifier.fillMaxWidth()) {
                        Spacer(Modifier.width(90.dp))
                        Button(onClick = { drive(DRIVE_SPEED, 0.0) }) { Text("▲ Fwd") }
                    }
                    Row(horizontalArrangement = Arrangement.spacedBy(8.dp), modifier = Modifier.fillMaxWidth()) {
                        Button(onClick = { drive(0.0, TURN_SPEED) }) { Text("◀ Left") }
                        Button(onClick = { stopBase() }) { Text("■ STOP") }
                        Button(onClick = { drive(0.0, -TURN_SPEED) }) { Text("Right ▶") }
                    }
                    Row(horizontalArrangement = Arrangement.spacedBy(8.dp), modifier = Modifier.fillMaxWidth()) {
                        Spacer(Modifier.width(90.dp))
                        Button(onClick = { drive(-DRIVE_SPEED, 0.0) }) { Text("▼ Back") }
                    }

                    Spacer(Modifier.height(4.dp))
                    Text(
                        "Hold nothing to coast; STOP sends BaseMotion.Stop. Real autonomy = the ported " +
                            "wandering + visual-servo policies.",
                        style = MaterialTheme.typography.bodySmall,
                    )
                }
            }
        }
    }

    override fun onStop() {
        scope.cancel()
    }

    companion object {
        private val logger: Logger = Logger.getLogger("AlienWanderingApp")
    }
}

/** Entry point referenced by the .nspkg component (`mainClass`). */
fun main() {
    val app = AlienWanderingApp()
    Runtime.getRuntime().addShutdownHook(Thread { app.close() })
    app.start()
}
