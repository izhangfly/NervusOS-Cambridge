package com.nervus.handteleop.app

import androidx.compose.ui.unit.dp
import com.nervus.handteleop.camera.Caller
import com.nervus.handteleop.camera.CameraSource
import com.nervus.handteleop.control.ArmLink
import com.nervus.handteleop.control.Teleop
import com.nervus.handteleop.control.TeleopSettings
import com.nervus.handteleop.ui.TeleopScreen
import com.nervus.handteleop.vision.HandEstimator
import com.nervus.handteleop.vision.Observation
import com.nervus.handteleop.vision.PoseEstimator
import com.nervus.handteleop.vision.Tracker
import com.nervus.sdk.component.ComponentConfig
import com.nervus.sdk.component.InterfaceRequirement
import com.nervus.sdk.component.NervusApp
import com.nervus.sdk.ui.attachComposeDesktop
import io.github.nervusos.ipc.v1.ControllerClass
import java.util.concurrent.atomic.AtomicReference
import java.util.logging.Logger

fun main() {
    val app = HandTeleopApp()
    Runtime.getRuntime().addShutdownHook(Thread { app.close() })
    app.start()
}

/**
 * Hand Teleop: drive the PiPER arm with your hand, through NervusOS.
 *
 *     front camera -> nervus.camerad -(Camera@1 + Transfer ring)-> this app
 *     YOLO pose + hand landmarks -> clutch mapping -> Manipulator@1 ServoToPose / SetGripper
 *     -> nervud (perm.manipulator.control, HUMAN ControlLease, deadman) -> nervus.piperd
 *     -> PiPER driver -> piper_sdk -> CAN -> arm
 */
class HandTeleopApp : NervusApp(
    config = ComponentConfig(componentId = "main", socketPath = "/run/nervus/nervud.sock"),
) {
    // Both resolve lazily and are optional: the app opens even without a camera or an arm and says
    // which one is missing, instead of failing to start.
    override val requiredInterfaces = listOf(
        InterfaceRequirement(
            id = CameraSource.CAMERA,
            resourceType = "nervus.resource.camera",
            labels = mapOf("nervus.camera.facing" to "front"),
            isRequired = false,
            resolveEagerly = false,
        ),
        InterfaceRequirement(id = ArmLink.ARM, isRequired = false, resolveEagerly = false),
    )

    private val caller = Caller { iface, method, payload, timeout -> call(iface, method, payload, timeout) }
    lateinit var pipeline: Pipeline
        private set

    override fun onReady() {
        pipeline = Pipeline(
            caller = caller,
            leaseFactory = {
                acquireControl("nervus.resource.manipulator.arm", "arm.main", ControllerClass.CONTROLLER_CLASS_HUMAN)
            },
            log = logger,
        )
        pipeline.start()
        attachComposeDesktop(title = "Hand Teleop", width = 1600.dp, height = 900.dp) {
            TeleopScreen(pipeline)
        }
    }

    override fun onStop() {
        if (::pipeline.isInitialized) pipeline.close()
    }
}

/** Camera, vision and control, each on its own thread so a slow stage never stalls the others. */
class Pipeline(
    caller: Caller,
    leaseFactory: () -> com.nervus.sdk.component.ControlLease,
    private val log: Logger,
) : AutoCloseable {
    val settings = TeleopSettings()
    val camera = CameraSource(caller, log)
    val teleop = Teleop(ArmLink(caller), leaseFactory, settings, log)
    private val latest = AtomicReference<Observation?>(null)

    @Volatile var visionError: String? = null
        private set
    @Volatile var visionFps: Float = 0f
        private set
    @Volatile private var running = false
    private val threads = ArrayList<Thread>()

    fun observation(): Observation? = latest.get()

    fun start() {
        running = true
        camera.start()
        threads += Thread({ visionLoop() }, "vision").apply { isDaemon = true; start() }
        threads += Thread({ controlLoop() }, "control").apply { isDaemon = true; start() }
    }

    private fun visionLoop() {
        val tracker = try {
            val threads = (Runtime.getRuntime().availableProcessors() / 2).coerceIn(2, 4)
            Tracker(
                PoseEstimator(resource("models/yolo26n-pose_320.onnx"), threads),
                HandEstimator(resource("models/hand_landmark.onnx"), threads),
            )
        } catch (e: Throwable) {
            visionError = "vision models failed to load: ${e.message}"
            log.severe(visionError)
            return
        }
        var lastSeq = -1L
        var count = 0
        var windowStart = System.nanoTime()
        while (running) {
            val frame = camera.latest()
            if (frame == null || frame.sequence == lastSeq) {
                Thread.sleep(3)
                continue
            }
            lastSeq = frame.sequence
            try {
                latest.set(tracker.process(frame))
                visionError = null
            } catch (e: Exception) {
                visionError = e.message
            }
            count++
            val now = System.nanoTime()
            if (now - windowStart > 1_000_000_000L) {
                visionFps = count * 1e9f / (now - windowStart)
                count = 0
                windowStart = now
            }
        }
    }

    private fun controlLoop() {
        val period = 50_000_000L // 20 Hz: the driver streams MOVE P at 20 Hz too
        var next = System.nanoTime()
        while (running) {
            val now = System.nanoTime()
            try {
                // A view of the hand older than 300 ms (camera or vision stalled) counts as no hand:
                // the arm must hold, not keep chasing a stale position.
                teleop.tick(latest.get()?.takeIf { now - it.atNanos < 300_000_000L }, now)
            } catch (e: Exception) {
                log.warning("control tick: ${e.message}")
            }
            next += period
            val sleep = next - System.nanoTime()
            if (sleep > 0) Thread.sleep(sleep / 1_000_000, (sleep % 1_000_000).toInt()) else next = System.nanoTime()
        }
        teleop.shutdown()
    }

    private fun resource(path: String): ByteArray =
        javaClass.classLoader.getResourceAsStream(path)?.use { it.readBytes() }
            ?: throw IllegalStateException("$path missing from the package")

    override fun close() {
        running = false
        teleop.stopNow()
        camera.close()
        threads.forEach { it.join(1500) }
    }
}
