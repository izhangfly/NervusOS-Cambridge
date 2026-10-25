package com.nervus.handteleop.app

import androidx.compose.ui.unit.dp
import androidx.compose.ui.window.Window
import androidx.compose.ui.window.application
import androidx.compose.ui.window.rememberWindowState
import com.nervus.handteleop.camera.CameraChoice
import com.nervus.handteleop.camera.Caller
import com.nervus.handteleop.camera.CameraSource
import com.nervus.handteleop.camera.FrameSource
import com.nervus.handteleop.camera.V4l2Source
import com.nervus.handteleop.control.Arm
import com.nervus.handteleop.control.ArmLink
import com.nervus.handteleop.control.DriverArm
import com.nervus.handteleop.control.OperationCaller
import com.nervus.handteleop.control.StartPoseStore
import com.nervus.handteleop.control.Teleop
import com.nervus.handteleop.control.TeleopSettings
import com.nervus.handteleop.control.TeleopStatus
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
import kotlin.system.exitProcess

fun main(args: Array<String>) {
    // Bring ONNX Runtime up before the window: created while Compose/Skia initialise on another thread,
    // its first session crashed the JVM inside ORT's logging on the Surface (SIGSEGV in ostream).
    ai.onnxruntime.OrtEnvironment.getEnvironment()
    if ("--standalone" in args) {
        val opts = try {
            StandaloneOptions.parse(args.toList())
        } catch (e: IllegalArgumentException) {
            System.err.println("hand-teleop: ${e.message}\n${StandaloneOptions.USAGE}")
            exitProcess(2)
        }
        runStandalone(opts)
        return
    }
    val app = HandTeleopApp()
    Runtime.getRuntime().addShutdownHook(Thread { app.close() })
    app.start()
}

/**
 * Standalone: an ordinary desktop window with no nervud, camerad or piperd in the path. Frames come
 * straight from a V4L2 device through ffmpeg, and the arm is driven through the PiPER driver's HTTP
 * API on loopback, as client "handteleop". The driver's one-client-at-a-time rule and its 0.4 s servo
 * deadman stand in for NervusOS's control lease.
 */
private fun runStandalone(opts: StandaloneOptions) {
    val log = Logger.getLogger("hand-teleop")
    val pipeline = Pipeline(V4l2Source(opts.camera, log), DriverArm(opts.driver, log = log), log, standalone = true)
    Runtime.getRuntime().addShutdownHook(Thread { pipeline.close() })
    pipeline.start()
    application {
        Window(onCloseRequest = { pipeline.close(); exitApplication() }, title = "Hand Teleop",
            state = rememberWindowState(width = 1600.dp, height = 900.dp)) {
            TeleopScreen(pipeline)
        }
    }
}

/** Command line for `--standalone`. */
data class StandaloneOptions(
    val camera: CameraChoice = CameraChoice.Named(V4l2Source.DEFAULT_NAME),
    val driver: String = DriverArm.DEFAULT_URL,
) {
    companion object {
        const val USAGE = "usage: hand-teleop --standalone [--camera /dev/videoN | --camera-name NAME] [--driver http://127.0.0.1:8770]"

        fun parse(args: List<String>): StandaloneOptions {
            var o = StandaloneOptions()
            var i = 0
            fun value(flag: String): String = args.getOrNull(++i)?.takeIf { it.isNotBlank() }
                ?: throw IllegalArgumentException("$flag needs a value")
            while (i < args.size) {
                when (val a = args[i]) {
                    "--standalone" -> {}
                    "--camera" -> o = o.copy(camera = CameraChoice.Path(value(a)))
                    "--camera-name" -> o = o.copy(camera = CameraChoice.Named(value(a)))
                    "--driver" -> o = o.copy(driver = value(a).trimEnd('/'))
                    else -> throw IllegalArgumentException("unknown option $a")
                }
                i++
            }
            // The driver has no authentication and moves the arm: talk to it on this machine only.
            val host = runCatching { java.net.URI(o.driver).host }.getOrNull()
            require(host == "127.0.0.1" || host == "localhost" || host == "::1") { "--driver must be a loopback address" }
            return o
        }
    }
}

/**
 * Hand Teleop: drive the PiPER arm with your hand, through NervusOS.
 *
 *     front camera -> nervus.camerad -(Camera@1 + Transfer ring)-> this app
 *     YOLO pose + hand landmarks -> clutch mapping -> Manipulator@1 ServoToPose / SetGripper
 *     -> nervud (perm.manipulator.control, HUMAN ControlLease, deadman) -> nervus.piperd
 *     -> PiPER driver -> piper_sdk -> CAN -> arm
 *
 * With `--standalone` the same pipeline runs as a plain desktop app instead (see [runStandalone]).
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
        val arm = ArmLink(
            caller,
            leaseFactory = {
                acquireControl("nervus.resource.manipulator.arm", "arm.main", ControllerClass.CONTROLLER_CLASS_HUMAN)
            },
            // The start-pose move is Manipulator@1 MoveJointTrajectory, a long task (an operation).
            operations = OperationCaller { iface, method, payload, timeout -> callOperation(iface, method, payload, timeout) },
        )
        pipeline = Pipeline(CameraSource(caller, logger), arm, logger)
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
    val camera: FrameSource,
    arm: Arm,
    private val log: Logger,
    /** Running without NervusOS (for the UI's labels). */
    val standalone: Boolean = false,
    /** Where "Set start pose here" saves (null: nowhere, for tests). */
    startPoseFile: java.io.File? = StartPoseStore.defaultFile(),
) : AutoCloseable {
    val settings = TeleopSettings()
    val armLabel = arm.label
    val teleop = Teleop(arm, settings, log, StartPoseStore(startPoseFile, log))
    private val latest = AtomicReference<Observation?>(null)

    @Volatile var visionError: String? = null
        private set
    @Volatile var visionFps: Float = 0f
        private set
    /** True while nobody is in view and the arm is not engaged: the models then run five times a second. */
    @Volatile var visionIdle: Boolean = false
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
            // Two threads per model: a full pass stays under the 50 ms the 20 Hz arm loop needs, and the app
            // no longer takes every core of the laptop (four threads each ran it at 355% CPU for hours).
            val threads = 2
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
        var lastPerson = System.nanoTime()
        var nextPass = 0L
        while (running) {
            val wait = nextPass - System.nanoTime()
            if (wait > 0) {
                Thread.sleep(wait / 1_000_000, (wait % 1_000_000).toInt())
                continue
            }
            val frame = camera.latest()
            if (frame == null || frame.sequence == lastSeq) {
                Thread.sleep(3)
                continue
            }
            lastSeq = frame.sequence
            val passStart = System.nanoTime()
            try {
                val obs = tracker.process(frame)
                latest.set(obs)
                if (obs.body != null) lastPerson = System.nanoTime()
                visionError = null
            } catch (e: Exception) {
                visionError = e.message
            }
            // The arm loop runs at 20 Hz, so tracking faster than that buys nothing. With nobody in view and the
            // arm not engaged, look five times a second until someone steps in.
            val engaged = teleop.status.phase != TeleopStatus.Phase.IDLE
            visionIdle = !engaged && System.nanoTime() - lastPerson > 3_000_000_000L
            nextPass = passStart + if (visionIdle) 200_000_000L else 50_000_000L
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

    // Called from both the window and a shutdown hook in standalone mode; the second call does nothing.
    private val closed = java.util.concurrent.atomic.AtomicBoolean(false)

    override fun close() {
        if (!closed.compareAndSet(false, true)) return
        running = false
        teleop.stopNow()
        camera.close()
        threads.forEach { it.join(1500) }
    }
}
