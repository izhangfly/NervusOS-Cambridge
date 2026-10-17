package com.nervus.piperring

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.ExperimentalMaterial3Api
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
import com.nervus.zring.sdk.RingSoundClient
import com.nervus.zring.sdk.startSensorReport
import com.nervus.zring.sdk.stopSensorReport
import com.nervus.zring.sdk.waitForGestureEvent
import com.nervus.zring.sdk.waitForSensorData
import io.github.nervusos.iface.manipulator.v1.MoveToPoseRequest
import io.github.nervusos.iface.manipulator.v1.Pose
import io.github.nervusos.iface.manipulator.v1.StopRequest
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import java.util.logging.Logger

/**
 * NervusOS app (L1): control the AgileX **PiPER** arm from the **Zilo ring**.
 *
 *   ring (BLE, CM390/RTL8761B) --nring--> RingSoundClient --IMU/gesture-->
 *     RingArmMapper --> Manipulator@1.MoveToPose --Port A--> nervud --Port B--> DimOS --> PiPER
 *
 * Move the hand → the gripper translates; dip the hand → the gripper's orientation follows
 * (gimbal). A wave gesture triggers a preset. Target device: RDK S100 (linux-arm64).
 */
class PiperRingApp(
    config: ComponentConfig = ComponentConfig(
        componentId = "piper_ring",
        socketPath = "/run/nervus/nervud.sock",
    ),
) : NervusApp(config) {

    override val requiredInterfaces = listOf(
        InterfaceRequirement(
            id = "nervus.interface.manipulator.arm",
            minMajor = 1,
            maxMajor = 1,
            resourceType = "nervus.resource.manipulator.arm",
            resourceRole = "main",
        )
    )

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    // UI-observable state
    private var ringStatus by mutableStateOf("ring: disconnected")
    private var mode by mutableStateOf("—")
    private var poseText by mutableStateOf("—")
    private var lastGesture by mutableStateOf("—")

    override fun onReady() {
        val arm = use(Manipulator::class)
        // TODO(VERIFY): AcquireControl a HUMAN lease on nervus.resource.manipulator.arm/main first.
        val ring = RingSoundClient(BlueZRingTransport())
        scope.launch { ringLoop(arm, ring) }
        scope.launch { gestureLoop(ring) }
        attachComposeDesktop(title = "Piper Ring Control", width = 460.dp, height = 360.dp) {
            PiperRingScreen(onStop = { stopArm(arm) })
        }
    }

    /** Connect the ring, stream IMU → arm poses. Reconnects on failure. */
    private suspend fun ringLoop(arm: Manipulator, ring: RingSoundClient) {
        while (scope.isActive) {
            try {
                ring.connect()
                ringStatus = "ring: connected"
                val start = ring.startSensorReport()
                val mapper = RingArmMapper(
                    start.accelerationRangeG.toDouble(),
                    start.gyroscopeRangeDPS.toDouble(),
                )
                ringStatus = "ring: streaming IMU"
                while (scope.isActive) {
                    val batch = ring.waitForSensorData()
                    var target: RingArmMapper.Target? = null
                    for (s in batch.samples) target = mapper.update(s)   // fold the batch
                    val t = target ?: continue
                    mode = "6-DOF · rot→gimbal · trans→XYZ · tilt %.0f°".format(t.tiltDeg)
                    poseText = "x=%.2f y=%.2f z=%.2f".format(t.pos[0], t.pos[1], t.pos[2])
                    arm.moveToPose(
                        MoveToPoseRequest.newBuilder()
                            .setTarget(
                                Pose.newBuilder().setFrame("base_link")
                                    .setX(t.pos[0]).setY(t.pos[1]).setZ(t.pos[2])
                                    .setQw(t.quat[0]).setQx(t.quat[1]).setQy(t.quat[2]).setQz(t.quat[3])
                                    .build()
                            )
                            .setMaxVelScale(0.3)
                            .build()
                    )
                }
            } catch (e: Exception) {
                ringStatus = "ring: ${e.message}"
                logger.warning("ring loop: ${e.message}")
                runCatching { ring.stopSensorReport() }
                delay(2000)
            }
        }
    }

    private suspend fun gestureLoop(ring: RingSoundClient) {
        while (scope.isActive) {
            try {
                val ev = ring.waitForGestureEvent()
                lastGesture = "gesture #${ev.gestureID}"
                // TODO: confirm which gestureID = "wave" on the device, then trigger a wave preset.
            } catch (e: Exception) {
                delay(500)
            }
        }
    }

    private fun stopArm(arm: Manipulator) {
        scope.launch { runCatching { arm.stop(StopRequest.newBuilder().build()) } }
    }

    @OptIn(ExperimentalMaterial3Api::class)
    @Composable
    private fun PiperRingScreen(onStop: () -> Unit) {
        MaterialTheme {
            Scaffold(topBar = { TopAppBar(title = { Text("Piper Ring Control") }) }) { pad ->
                Column(
                    modifier = Modifier.fillMaxSize().padding(pad).padding(16.dp),
                    verticalArrangement = Arrangement.spacedBy(12.dp),
                ) {
                    Card(modifier = Modifier.fillMaxWidth()) {
                        Text(
                            "ring → Manipulator@1 → NervusOS → DimOS → PiPER\n\n" +
                                "$ringStatus\nmode: $mode\ntarget: $poseText\n$lastGesture",
                            modifier = Modifier.padding(12.dp),
                        )
                    }
                    Button(onClick = onStop, modifier = Modifier.fillMaxWidth()) { Text("■ STOP arm") }
                    Text(
                        "Move hand → translate the gripper; dip hand → gimbal. Needs the CM390 dongle " +
                            "(RTL8761B) + nervud + the DimOS manipulator provider.",
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
        private val logger: Logger = Logger.getLogger("PiperRingApp")
    }
}

/** Entry point referenced by the .nspkg component (`mainClass`). */
fun main() {
    val app = PiperRingApp()
    Runtime.getRuntime().addShutdownHook(Thread { app.close() })
    app.start()
}
