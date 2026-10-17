package com.nervus.piperring

import com.nervus.sdk.component.ComponentConfig
import com.nervus.sdk.component.InterfaceRequirement
import com.nervus.sdk.component.NervusApp
import io.github.nervusos.iface.manipulator.v1.MoveToPoseRequest
import io.github.nervusos.iface.manipulator.v1.Pose
import io.github.nervusos.iface.manipulator.v1.SetGripperRequest
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import java.io.BufferedReader
import java.io.InputStreamReader
import java.net.Socket
import java.util.logging.Logger

/**
 * NervusOS app (L1): drives the AgileX PiPER from the Zilo ring.
 *
 * Data flow:
 *   ring_bridge.py (BLE + IMU fusion) --JSON intents/TCP--> this app
 *      --Manipulator@1 over Port A--> nervud (arbiter, HUMAN lease)
 *      --Port B--> DimOS coordinator-cartesian-ik-piper --> can0 --> PiPER
 *
 * The ring's *orientation* drives the tip (gimbal); a leaky-integrated *translation*
 * target drives XYZ. Both arrive as absolute `pose` intents already in meters +
 * quaternion, so this app just streams MoveToPose at 20 Hz (leashed by the sidecar's
 * workspace clamp). A `wave` gesture triggers a built-in preset.
 *
 * !! CANNOT VERIFY here (nervud not on the RDK yet) — see README "Verify later":
 *   - AcquireControl lease (envelope.proto oneof #70) — the exact SDK surface.
 *   - InterfaceProxy marshalling of the proto request/response.
 *   - The Port B binding Manipulator.MoveToPose -> DimOS cartesian command.
 */
class PiperRingApp(
    config: ComponentConfig = ComponentConfig(componentId = "piper_ring"),
    private val sidecarHost: String = "127.0.0.1",
    private val sidecarPort: Int = 8790,
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

    @Volatile private var latestPos: DoubleArray? = null
    @Volatile private var latestQuat: DoubleArray? = null
    @Volatile private var gripperOpen = true

    override fun onReady() {
        val arm = use(Manipulator::class)
        // TODO(VERIFY): acquire a HUMAN control lease on nervus.resource.manipulator.arm/main
        //   before commanding. AcquireControl is an IPC/envelope message (oneof #70); the
        //   returned AcquireControlSuccess.lease_id must ride in each call's ExecutionContext.
        //   Confirm whether Component/NervusClient exposes acquireControl(...) or if the
        //   resolveEndpoint(resourceRole="main") above already establishes the lease.
        logger.info("piper_ring ready; connecting ring sidecar $sidecarHost:$sidecarPort")
        scope.launch { readSidecar() }
        scope.launch { controlLoop(arm) }
    }

    /** Stream the latest ring pose to the arm at 20 Hz (idempotent MoveToPose goals). */
    private suspend fun controlLoop(arm: Manipulator) {
        while (scope.isActive) {
            val p = latestPos
            val q = latestQuat
            if (p != null && q != null) {
                val pose = Pose.newBuilder()
                    .setFrame("base_link")
                    .setX(p[0]).setY(p[1]).setZ(p[2])
                    .setQw(q[0]).setQx(q[1]).setQy(q[2]).setQz(q[3])
                    .build()
                try {
                    arm.moveToPose(
                        MoveToPoseRequest.newBuilder().setTarget(pose).setMaxVelScale(0.3).build()
                    )
                } catch (e: Exception) {
                    logger.warning("moveToPose failed: ${e.message}")
                }
            }
            delay(50)
        }
    }

    /** Connect to ring_bridge.py and parse newline-JSON intents. */
    private fun readSidecar() {
        while (scope.isActive) {
            try {
                Socket(sidecarHost, sidecarPort).use { s ->
                    BufferedReader(InputStreamReader(s.getInputStream())).use { r ->
                        var line = r.readLine()
                        while (line != null) {
                            handleIntent(line)
                            line = r.readLine()
                        }
                    }
                }
            } catch (e: Exception) {
                logger.warning("ring sidecar disconnected (${e.message}); retrying in 1s")
                Thread.sleep(1000)
            }
        }
    }

    /**
     * Minimal, dependency-free JSON extraction. VERIFY: swap for kotlinx.serialization
     * once the sidecar protocol is frozen. Intent shapes are defined in ring_bridge.py.
     */
    private fun handleIntent(line: String) {
        val t = TYPE_RE.find(line)?.groupValues?.get(1) ?: return
        when (t) {
            "pose" -> {
                val nums = NUM_RE.findAll(line).map { it.value.toDouble() }.toList()
                if (nums.size >= 7) {          // pos[0..2] then quat[0..3], in JSON order
                    latestPos = doubleArrayOf(nums[0], nums[1], nums[2])
                    latestQuat = doubleArrayOf(nums[3], nums[4], nums[5], nums[6])
                }
            }
            "gripper" -> {
                gripperOpen = line.contains("\"open\":true") || line.contains("\"open\": true")
                scope.launch { setGripper(gripperOpen) }
            }
            "gesture" -> if (line.contains("wave")) scope.launch { doWave() }
        }
    }

    private suspend fun setGripper(open: Boolean) {
        try {
            use(Manipulator::class).setGripper(
                SetGripperRequest.newBuilder().setOpening(if (open) 1.0 else 0.0).build()
            )
        } catch (e: Exception) {
            logger.warning("setGripper failed: ${e.message}")
        }
    }

    /**
     * Built-in "wave" preset for the demo (webcam/ring wave -> arm waves). Placeholder:
     * fill with a short MoveToPose/MoveJointTrajectory sequence once poses are calibrated.
     */
    private suspend fun doWave() {
        logger.info("wave gesture received (preset TODO)")
    }

    override fun onStop() {
        scope.cancel()
    }

    companion object {
        private val logger: Logger = Logger.getLogger("PiperRingApp")
        private val TYPE_RE = Regex("\"t\"\\s*:\\s*\"(\\w+)\"")
        private val NUM_RE = Regex("-?\\d+\\.?\\d*(?:[eE][-+]?\\d+)?")
    }
}

/** Entry point used by the .nspkg thin JAR (Main-Class). */
fun main() {
    val app = PiperRingApp()
    app.start()
    Runtime.getRuntime().addShutdownHook(Thread { app.close() })
    Thread.currentThread().join()
}
