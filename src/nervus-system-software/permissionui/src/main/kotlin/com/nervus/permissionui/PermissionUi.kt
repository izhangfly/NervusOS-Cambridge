package com.nervus.permissionui

import androidx.compose.ui.unit.dp
import com.nervus.sdk.annotations.Method
import com.nervus.sdk.component.ComponentConfig
import com.nervus.sdk.component.InterfaceRequirement
import com.nervus.sdk.component.NervusApp
import com.nervus.sdk.component.ProvidedInterface
import com.nervus.sdk.operation.OperationPending
import com.nervus.sdk.operation.currentCaller
import com.nervus.sdk.operation.currentOperationId
import com.nervus.sdk.ui.NervusWindow
import com.nervus.sdk.ui.attachComposeDesktop
import com.nervus.sysui.X11WindowControl
import io.github.nervusos.iface.permission.v1.GrantList
import io.github.nervusos.iface.permission.v1.GrantState
import io.github.nervusos.iface.permission.v1.ListGrantsRequest
import io.github.nervusos.iface.permission.v1.OpenManagerRequest
import io.github.nervusos.iface.permission.v1.PackageGrants
import io.github.nervusos.iface.permission.v1.PermissionGrant
import io.github.nervusos.iface.permission.v1.PermissionRequestDecision
import io.github.nervusos.iface.permission.v1.PermissionUiErrorDetail
import io.github.nervusos.iface.permission.v1.PermissionUiReason
import io.github.nervusos.iface.permission.v1.RequestPermissionOutcome
import io.github.nervusos.iface.permission.v1.RequestPermissionRequest
import io.github.nervusos.iface.permission.v1.RequestPermissionResult
import io.github.nervusos.iface.permission.v1.SetGrantStateRequest
import io.github.nervusos.iface.permission.v1.SetGrantStateResponse
import io.github.nervusos.ipc.v1.StatusCode
import io.github.nervusos.ipc.v1.StdInterfaceSchema
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import java.util.logging.Logger
import kotlin.system.exitProcess

/**
 * Window title.
 *
 * **It must match the title given to [attachComposeDesktop]**: `OpenManager` uses it to bring the window
 * to the front (X11 finds windows by title). Two different strings raise no compile or run-time error;
 * the symptom is "I pressed permission manager in Settings and nothing came up", while the filter state did change,
 * so it looks like a random failure. A constant makes it impossible to get wrong.
 */
const val WINDOW_TITLE = "Permissions"

/**
 * How long to wait for the user to answer a permission request (minutes).
 *
 * A limit rather than waiting forever: the user may never answer (walked away, window covered by something else).
 * Waiting forever would hold that operation and the worker thread for good, and the serialising gate would never
 * open again, so every later request would be BUSY: "permission requests never pop up any more".
 *
 * A timeout counts as [deny], the same direction as failing to show the dialog: if we couldn't ask, the answer is no.
 *
 * 5 minutes is shorter than the SDK's default requestPermission timeout (10 minutes), so normally
 * this side sends a clear "denied" before the caller hits its own timeout. The caller's timeout gives it
 * no per-permission result, only "it failed".
 */
private const val USER_ANSWER_TIMEOUT_MINUTES = 5L

/** Shorthand: the proto's decision enum names are long, and spelling them out in matches reads worse. */
private typealias Decision = PermissionRequestDecision

/**
 * A permission request waiting for the user's answer.
 *
 * [answer] is how the UI hands back the answer: the worker thread waits on it with a limit, and the UI thread
 * completes it after the user presses a button. A CompletableFuture rather than another StateFlow,
 * since the worker would have to poll that, and what it waits for is a one-off event.
 */
data class PendingRequest(
    /** The requester's package ID. **The trusted identity from the kernel**, not what the request claims. */
    val callerPackageId: String,
    /** The requester's display name (the manifest label); may be empty. */
    val callerLabel: String,
    /**
     * The reason the app gives for its request.
     *
     * [Untrusted text.] The UI must say clearly that it comes from the app, and it must not look like system text,
     * or "The system requires you to grant camera access" in forged wording would fool users.
     */
    val rationale: String,
    /** The permissions to ask about, with the kernel's text and risk levels. */
    val items: List<PermissionGrant>,
    val answer: java.util.concurrent.CompletableFuture<Set<String>>,
)

/** Builds one per-permission result. */
private fun outcome(
    permissionId: String,
    granted: Boolean,
    decision: PermissionRequestDecision,
): RequestPermissionOutcome =
    RequestPermissionOutcome.newBuilder()
        .setPermissionId(permissionId)
        .setGranted(granted)
        .setDecision(decision)
        .build()

/** Builds this interface's typed error detail. */
private fun uiErrorDetail(reason: PermissionUiReason): ByteArray =
    PermissionUiErrorDetail.newBuilder().setReason(reason).build().toByteArray()

/**
 * Method IDs of `nervus.interface.permission.admin`.
 *
 * The values come from the `PermissionAdminMethod` enum in
 * `nervus-ipc/proto/nervus/interface/permission/v1/permission.proto`, and **the proto wins**: its
 * `method_meta.method_id` must match the enum numbers (the registry checks this on extraction
 * and fails closed on a mismatch).
 *
 * Hand-written constants rather than the generated enum, for the same reason as the launcher's PkgManagerMethods:
 * the generated enum reads awkwardly, and if the two ever disagree the call gets a precise NOT_FOUND
 * instead of silently calling the wrong method.
 */
object PermissionAdmin {
    const val INTERFACE_ID = "nervus.interface.permission.admin"

    const val LIST_GRANTS = 1
    const val SET_GRANT_STATE = 2
}

/**
 * The interface this app **exports**: `nervus.interface.permission.ui`.
 *
 * These values also follow the proto's `PermissionUiMethod` enum.
 */
object PermissionUiInterface {
    const val INTERFACE_ID = "nervus.interface.permission.ui"
    const val MAJOR = 1

    const val CONFIRM_INSTALL = 1
    const val OPEN_MANAGER = 2
    const val REQUEST_PERMISSION = 3
}

/**
 * Server side of `permission.ui`.
 *
 * Only `OPEN_MANAGER` is implemented. **`CONFIRM_INSTALL` is deliberately left out**: showing and confirming
 * permissions at install time belongs to the package installer, a different package. "Can grant permissions" and "can install
 * any software" are two powerful abilities, and one process holding both would make a target that can do both.
 * The kernel answers unimplemented methods with a precise NOT_FOUND, so nothing goes to the wrong place silently.
 */
class PermissionUiEndpoint(private val app: PermissionUi) {

    /**
     * Opens the permission manager, optionally showing just one package.
     *
     * Does two things: records "whose permissions to show" so the UI narrows, then brings the window to the front.
     *
     * **Both are needed.** Recording the state without activating the window makes it look to the caller (Settings) as if nothing happened:
     * Nervus shows one foreground window, so this app might be under Settings, or the kernel may have just
     * started it on demand and its window isn't up yet.
     *
     * Returns an empty ByteArray rather than Unit: this method's `response_type` is empty, so on the wire it's
     * a success with no payload. **Returning Unit makes the SDK send `"kotlin.Unit"` as the payload via
     * toString**: it compiles and doesn't fail at once,
     * but it's plainly the wrong bytes on the wire.
     */
    @Method(id = PermissionUiInterface.OPEN_MANAGER)
    fun openManager(req: OpenManagerRequest): ByteArray {
        app.onOpenManager(req.packageId)
        return ByteArray(0)
    }

    /**
     * Asks the user for a set of permissions (for the requester itself).
     *
     * # The requester's identity comes from the kernel, not the request
     *
     * [RequestPermissionRequest] has **no** package_id field; the identity comes from
     * [currentCaller], which is what nervud worked out from the connection's credentials. With such a field
     * any app could request permissions for another, and the package name the user sees in the dialog would be the one being impersonated.
     *
     * # Why it returns [OperationPending] straight away
     *
     * This method waits for the user to read and press a button, and **the SDK's dispatch runs on the read loop**
     * (`NervusClient.handleDispatch` is a synchronous call). Blocking here for the user means no more frames
     * arrive, including the replies to `acceptOperation` / `completeOperation` themselves.
     * That's a guaranteed deadlock, not an occasional race.
     *
     * So: inside the handler, **take** the operation id and the caller **synchronously**, hand them to the worker thread,
     * and return Pending at once. Both must be read here, because they live in ThreadLocals
     * the worker thread can't see (see the SDK's CurrentOperation / CurrentCaller).
     */
    @Method(id = PermissionUiInterface.REQUEST_PERMISSION)
    fun requestPermission(req: RequestPermissionRequest): OperationPending {
        app.onRequestPermission(
            operationId = currentOperationId(),
            callerPackageId = currentCaller().packageId,
            permissionIds = req.permissionIdsList.toList(),
            rationale = req.rationale,
        )
        return OperationPending.INSTANCE
    }
}

/**
 * The permission manager app.
 *
 * It is the [only] UI in the system that can change the runtime grant of USER_CONSENT permissions.
 *
 * # Why it's a separate package
 *
 * `perm.permission.admin` requires the platform-release signing role, and Settings, the desktop and the file manager
 * all sign platform-systemapp, so they can't have it. That's not a configuration slip: "the power to give any app the camera and
 * motion control" shouldn't share a signing chain with a large package that changes often.
 * Android makes PermissionController a separate APK for the same reason.
 *
 * # It makes no decisions itself
 *
 * Which permissions to list, whether they can change and what state results are all decided by nervud. This app shows the
 * result and turns what the user wants into a call. Same reasoning as settings-app: copying the
 * checks into the UI only creates a second source of truth, and once they disagree a switch the user can flip is bound to fail.
 *
 * In particular, **the kernel only returns the USER_CONSENT tier**. Other grant modes have no runtime state,
 * so a switch that can't move never appears here and the UI needs no filtering.
 */
class PermissionUi(config: ComponentConfig) : NervusApp(config) {

    override val requiredInterfaces: List<InterfaceRequirement> = listOf(
        InterfaceRequirement(
            id = PermissionAdmin.INTERFACE_ID,
            // Required: a built-in interface, present while nervud is alive. Failing to resolve means a kernel version mismatch
            // (this package declares min_nervus_api 2), and opening a blank permissions page would be worse than failing
            // to start: the user would think "no app has asked for sensitive permissions"
            isRequired = true,
        ),
    )

    /**
     * The interface this app exports.
     *
     * schemaHash [must] come from [StdInterfaceSchema]: the kernel compares the one sent with RegisterEndpoint
     * byte for byte against the Catalog's, and refuses a mismatch or an empty value
     * (nervud internal/endpoint/register.go). The JVM side can't compute it: the hash is
     * sha256(deterministically encoded FileDescriptorSet), and protobuf-java doesn't guarantee the same bytes as Go's
     * deterministic encoding. So Go computes it once and writes it out, and both sides read the same generated file.
     *
     * This must also match the manifest's `exports` and the interface declared in provider.binpb
     * exactly: the catalog's addArtifacts closes both ways, and missing any one of the three makes
     * the whole Catalog build fail (not just this package being quarantined).
     */
    override val providedInterfaces: List<ProvidedInterface> = listOf(
        ProvidedInterface(
            id = PermissionUiInterface.INTERFACE_ID,
            major = PermissionUiInterface.MAJOR,
            instance = PermissionUiEndpoint(this),
            schemaHash = StdInterfaceSchema.hashOf(
                PermissionUiInterface.INTERFACE_ID,
                PermissionUiInterface.MAJOR,
            ),
        ),
    )

    private val log = Logger.getLogger(PermissionUi::class.java.name)

    /**
     * The "show only this package" filter; the empty string means the overview.
     *
     * A [MutableStateFlow] rather than Compose's mutableStateOf: the IPC
     * dispatch thread writes it and Compose's UI thread reads it. Compose state must be written
     * on the main thread; writing it from another thread gives an intermittent error that depends on timing.
     */
    private val _managerFilter = MutableStateFlow("")
    val managerFilter: StateFlow<String> get() = _managerFilter

    /**
     * Handles an incoming `OpenManager`: narrow the filter and bring the window to the front.
     *
     * Called by [PermissionUiEndpoint] on the dispatch thread. **It makes no decisions**:
     * whether the caller may open this screen is decided by the kernel from `required_permission`;
     * this only carries it out.
     *
     * # Why state updates synchronously and the window work runs asynchronously
     *
     * The SDK's dispatch runs on the read loop (`NervusClient.handleDispatch` is synchronous).
     * `activateWindow` can take up to 50 × (2 s timeout + 100 ms sleep) ≈ 105 s, and waiting for it here
     * would hold the whole read loop: this process couldn't even send its own DispatchResult,
     * and the caller (Settings) would get a TimeoutException after 30 s.
     *
     * This mirrors [onRequestPermission] exactly: the dispatch thread only does plain in-memory assignments (fast),
     * and everything involving I/O or blocking goes to the worker thread.
     */
    fun onOpenManager(packageId: String) {
        log.info("openManager packageId='${packageId.ifEmpty { "(all)" }}'")
        // The user asked to see this screen (came from Settings), so it mustn't be taken away when a request finishes.
        // [These two lines must run synchronously on the dispatch thread]: they're plain memory writes and don't block.
        // On the worker thread they would run after the handler returns (after DispatchResult is sent),
        // leaving a window where "Settings got success but the permissions screen state hasn't changed yet".
        windowWanted = true
        _managerFilter.value = packageId
        // [window.show + activateWindow run asynchronously].
        //
        // window.show() is just a StateFlow assignment and very fast, but activateWindow can block
        // for ~105 s. Putting both on the worker thread keeps show() before activate,
        // and lets the dispatch thread return at once.
        requestWorker.execute {
            // [show is required]: the window may be hidden (it stepped back after the last permission request was answered).
            // In that case the process is alive and the endpoint valid, so the kernel won't start it again, and without show
            // nothing would seem to happen.
            window.show()
            // A failed activation is only logged, not thrown: the filter is already set, and switching to this window by hand
            // still shows the right content. Failing the whole call over one window activation would be out of proportion.
            if (!X11WindowControl.activateWindow(WINDOW_TITLE)) {
                log.warning("openManager: cannot activate window '$WINDOW_TITLE'")
            }
        }
    }

    /**
     * Back to the overview. Called by the "All apps' permissions" link in the UI.
     *
     * This exit is needed: after `OpenManager` arrives with a package ID the screen is narrowed, and this app has
     * no other navigation. Without it, someone who came from Settings is stuck in the single-package view and has to close
     * the window and reopen it from the desktop.
     */
    fun clearManagerFilter() {
        _managerFilter.value = ""
    }

    /**
     * The permission request currently being asked; null means none.
     *
     * A [MutableStateFlow] for the same reason as [managerFilter]: the worker thread writes it and
     * Compose's UI thread reads it.
     */
    private val _pendingRequest = MutableStateFlow<PendingRequest?>(null)
    val pendingRequest: StateFlow<PendingRequest?> get() = _pendingRequest

    /**
     * Whether the user **chose** to look at this screen.
     *
     * Decides whether to put the window away after a permission request finishes. This app is on-demand: a plain
     * permission request starts it, and its window is fullscreen ([attachComposeDesktop] uses
     * `Maximized`). Left in front after the answer, the user would have to press Esc to get out, when they never
     * meant to open the permission manager; they just pressed a button in another app.
     *
     * The opposite is worse: taking the window away while the user **is** using it pulls out what they're
     * looking at. So only a window that came up for this request gets put away.
     *
     * Set to true in two cases: arriving from Settings (`OpenManager`), or the user flipping
     * any switch in the manager. Both mean they're using this screen.
     *
     * @Volatile: the dispatch thread and the UI thread write it, and the permission request's worker thread reads it.
     */
    @Volatile
    private var windowWanted = false

    /**
     * This app's window handle.
     *
     * [main] passes it to `attachComposeDesktop`, which attaches it to the real window. It tells "hide myself"
     * from "close myself" apart: **hiding must never end the process**, or the endpoint this component registered
     * dies while callers still have it cached.
     */
    val window = NervusWindow()

    /**
     * The user flipped a switch in the manager: from now on this window is wanted.
     *
     * Called by the UI when it changes a grant. **Setting it only in [onOpenManager] isn't enough**: the user may also
     * open this app straight from the desktop, and that path involves no IPC call through us.
     */
    fun markWindowWanted() {
        windowWanted = true
    }

    /**
     * The gate that serialises requests. **Only one request is asked at a time**.
     *
     * With two confirmation dialogs open the user can't tell whom they're granting to, and the consequence here is granting
     * permissions to the wrong app. A second concurrent request fails with BUSY and the caller retries later.
     */
    private val requestGate = java.util.concurrent.Semaphore(1)

    /**
     * The worker thread that runs the dialog flow.
     *
     * **It must be off the dispatch thread**: the SDK's dispatch runs on the read loop, and blocking there for
     * the user would mean not even receiving the reply to our own `completeOperation`: a guaranteed deadlock.
     *
     * A single thread rather than a pool: concurrent requests are stopped by [requestGate], and extra threads would only
     * make "two dialogs at once" possible. A daemon thread: process exit shouldn't hang on
     * a dialog nobody answers.
     */
    private val requestWorker = java.util.concurrent.Executors.newSingleThreadExecutor { r ->
        Thread(r, "permission-request").apply { isDaemon = true }
    }

    /**
     * Handles an incoming `RequestPermission`. **Returns at once**; the real work happens on the worker thread.
     *
     * @param operationId taken by the handler on the dispatch thread and passed in, because it
     * lives in a ThreadLocal the worker thread can't read.
     * @param callerPackageId the requester identity the kernel worked out, **not** what the request claims.
     */
    fun onRequestPermission(
        operationId: Long,
        callerPackageId: String,
        permissionIds: List<String>,
        rationale: String,
    ) {
        requestWorker.execute {
            runRequest(operationId, callerPackageId, permissionIds, rationale)
        }
    }

    /**
     * Runs one permission request to completion on the worker thread.
     *
     * # Whatever path it takes, the operation must end
     *
     * The caller sits in `callOperation` waiting for a final state. Miss one complete here and it waits until
     * its own timeout (SDK default: 10 minutes), a failure that looks like "the system hung" when the real
     * cause is just an early return from this function. So the whole thing is in try/catch, and the exception path also sends a final state.
     */
    private fun runRequest(
        operationId: Long,
        callerPackageId: String,
        permissionIds: List<String>,
        rationale: String,
    ) {
        try {
            // An empty identity must be refused. Treating it as "trusted local" would hand a connection whose caller wasn't identified
            // a dialog that can change permissions, and the package name the user saw would be blank.
            //
            // [The code is FAILED_PRECONDITION, not UNAUTHENTICATED]: the latter isn't on
            // nervud's providerFailCode allowlist (authorisation decisions can only come from the kernel,
            // and a Provider claiming one is a violation), so returning it would be normalised fail-closed to INTERNAL
            // and audited as a contract violation, leaving the caller with an even vaguer reason.
            if (callerPackageId.isEmpty()) {
                log.warning("requestPermission: rejected, caller identity unknown")
                failOperation(operationId, StatusCode.STATUS_CODE_FAILED_PRECONDITION)
                return
            }
            if (permissionIds.isEmpty()) {
                failOperation(operationId, StatusCode.STATUS_CODE_INVALID_ARGUMENT)
                return
            }
            // tryAcquire rather than acquire: if we can't get it, fail with BUSY instead of queueing. Queueing would
            // make the caller wait an unpredictable time, and its own timeout would still fire
            if (!requestGate.tryAcquire()) {
                log.info("requestPermission: busy, rejecting request from $callerPackageId")
                failOperation(
                    operationId,
                    StatusCode.STATUS_CODE_FAILED_PRECONDITION,
                    uiErrorDetail(PermissionUiReason.PERMISSION_UI_REASON_BUSY),
                )
                return
            }
            try {
                askUser(operationId, callerPackageId, permissionIds, rationale)
            } finally {
                _pendingRequest.value = null
                requestGate.release()
            }
        } catch (e: Exception) {
            // Safety net: any unexpected failure must end the operation, or the caller waits forever
            log.severe("requestPermission failed for $callerPackageId: ${e.message}")
            runCatching { failOperation(operationId, StatusCode.STATUS_CODE_INTERNAL) }
        }
    }

    /**
     * Classify, show the dialog, store, send the final state.
     *
     * # Permission text and whether a permission can be requested both come from the kernel
     *
     * `ListGrants(package_id = requester)` returns "the USER_CONSENT permissions this package actually got at install time",
     * with their text and current state. So:
     *
     *   listed + not granted        → ask the user
     *   listed + granted            → already has it, no dialog
     *   listed + permanently denied → no dialog (asking can't change the outcome)
     *   **not listed**              → can't be requested: maybe never asked for in the manifest,
     *                                 refused at install, or not in the USER_CONSENT tier
     *
     * The last line treats "the kernel didn't list it" as "can't be requested". That way this app doesn't need to know
     * about grant modes, trust levels or signing roles; those all live in nervud, and copying them into the UI
     * would only create a second source of truth.
     */
    private fun askUser(
        operationId: Long,
        callerPackageId: String,
        permissionIds: List<String>,
        rationale: String,
    ) {
        val pkg = listGrants().firstOrNull { it.packageId == callerPackageId }
        val grantable = pkg?.permissionsList?.associateBy { it.permissionId } ?: emptyMap()

        val outcomes = mutableMapOf<String, RequestPermissionOutcome>()
        val toAsk = mutableListOf<PermissionGrant>()

        for (id in permissionIds.distinct()) {
            val grant = grantable[id]
            when {
                grant == null ->
                    outcomes[id] = outcome(id, false, Decision.PERMISSION_REQUEST_DECISION_NOT_REQUESTABLE)

                grant.state == GrantState.GRANT_STATE_GRANTED ->
                    outcomes[id] = outcome(id, true, Decision.PERMISSION_REQUEST_DECISION_GRANTED)

                grant.state == GrantState.GRANT_STATE_DENIED_PERMANENT ->
                    outcomes[id] = outcome(id, false, Decision.PERMISSION_REQUEST_DECISION_DENIED_PERMANENT)

                else -> toAsk += grant
            }
        }

        // Nothing to ask: return the result without a dialog. Popping an empty window for a request that already
        // has everything (or can't get anything) is pure nuisance for the user
        if (toAsk.isEmpty()) {
            completeRequest(operationId, outcomes.values)
            return
        }

        val answer = java.util.concurrent.CompletableFuture<Set<String>>()
        _pendingRequest.value = PendingRequest(
            callerPackageId = callerPackageId,
            callerLabel = pkg?.label.orEmpty(),
            rationale = rationale,
            items = toAsk.toList(),
            answer = answer,
        )
        // Make the window visible first, then bring it to the front.
        //
        // Both steps are needed: window.show() handles Compose's visible flag (it was hidden after
        // the last request was answered), and activateWindow handles focus in the window manager. Without the first,
        // wmctrl can't activate a hidden window; without the second, the window is visible but under the others.
        window.show()

        // Bring the window to the front. If that fails, treat it as "couldn't ask the user" and deny everything.
        // [Never let it through silently]: granting permissions when the confirmation can't be shown means quietly
        // pressing Allow on the user's behalf
        if (!X11WindowControl.activateWindow(WINDOW_TITLE)) {
            log.warning("requestPermission: cannot activate window, treating as denied")
            for (grant in toAsk) {
                outcomes[grant.permissionId] =
                    outcome(grant.permissionId, false, Decision.PERMISSION_REQUEST_DECISION_DENIED)
            }
            completeRequest(operationId, outcomes.values)
            return
        }

        // Bounded wait: the user may never answer (walked away, window covered by something else). Waiting forever would
        // hold this operation and the worker thread for good, and the gate would never open again, so the next request
        // would always be BUSY. A timeout counts as "deny", the same direction as failing to show the dialog
        val accepted = try {
            answer.get(USER_ANSWER_TIMEOUT_MINUTES, java.util.concurrent.TimeUnit.MINUTES)
        } catch (_: java.util.concurrent.TimeoutException) {
            log.info("requestPermission: user did not answer in time, treating as denied")
            emptySet()
        }

        for (grant in toAsk) {
            val id = grant.permissionId
            if (id !in accepted) {
                outcomes[id] = outcome(id, false, Decision.PERMISSION_REQUEST_DECISION_DENIED)
                continue
            }
            // The user pressed Allow, but **the state read back after storing wins** over echoing their click:
            // SetGrantState can still fail (the package was just uninstalled, the permission just downgraded). Returning
            // granted=true when it didn't take effect would send the caller straight into PERMISSION_DENIED
            val state = runCatching { setGrantState(callerPackageId, id, true) }.getOrElse { e ->
                log.warning("requestPermission: grant $id failed: ${e.message}")
                GrantState.GRANT_STATE_UNSPECIFIED
            }
            val ok = state == GrantState.GRANT_STATE_GRANTED
            outcomes[id] = outcome(
                id, ok,
                if (ok) Decision.PERMISSION_REQUEST_DECISION_GRANTED
                else Decision.PERMISSION_REQUEST_DECISION_DENIED,
            )
        }
        completeRequest(operationId, outcomes.values)
    }

    private fun completeRequest(operationId: Long, outcomes: Collection<RequestPermissionOutcome>) {
        // A fixed order (by permission ID), as the proto specifies: the caller indexes by ID,
        // but a stably sorted result keeps its logs comparable
        val result = RequestPermissionResult.newBuilder()
            .addAllOutcomes(outcomes.sortedBy { it.permissionId })
            .build()

        // [Put the window away before completeOperation]. completeOperation is what wakes the requester:
        // it returns from callOperation and may well bring its own window to the front at once.
        // Calling hideActiveWindow after that would race it for the same "active window", and that function
        // minimises [whichever window is active at that moment]; losing the race would minimise the requester's window.
        //
        // Here, the requester is still blocked in callOperation and can't take focus,
        // so hideActiveWindow is sure to act on us.
        hideWindowIfUnwanted()

        // [A user's denial also uses completeOperation, not failOperation]: it's a confirmation that finished normally,
        // and the answer was "no". A failure code would leave the caller unable to tell "the user said no" from
        // "the confirmation screen crashed", and those deserve completely different reactions in the UI
        //
        // acceptOperation must come first: the state machine doesn't allow PENDING straight to SUCCEEDED
        acceptOperation(operationId)
        completeOperation(operationId, result.toByteArray())
    }

    /**
     * Puts the window away after a request is answered, **only if** it came up for this request.
     *
     * Hide rather than exit: this component is on-demand and the kernel reaps it when idle
     * (the manifest's `idle_timeout_sec`). Exiting ourselves would make the next request pay another
     * Compose cold start, during which the user sees nothing happen for a second or two.
     *
     * Failures are only logged: the request's result is already stored, and a failed window operation shouldn't affect it.
     */
    private fun hideWindowIfUnwanted() {
        if (windowWanted) return
        // [Use window.hide(), not an X11 minimise].
        //
        // This used to call X11WindowControl.hideActiveWindow() (xdotool windowminimize),
        // and here a minimise makes Compose fire onCloseRequest, so "hide myself"
        // actually [killed this process]. The symptom was far from the cause: the process was gone, the endpoint Settings had cached
        // was dead (NOT_FOUND), and once a new Resolve started it again it popped up a fresh fullscreen window, so
        // the user saw "the permissions window won't close".
        //
        // window.hide() uses Compose's own visible flag, bypassing the window manager, so the process stays
        // and its registered endpoint stays valid.
        window.hide()
    }

    /** The UI hands the user's choice back to the worker thread. accepted holds the permission IDs the user ticked. */
    fun resolvePendingRequest(accepted: Set<String>) {
        _pendingRequest.value?.answer?.complete(accepted)
    }

    /** Lists each package's grantable permissions and their state. The kernel has already sorted them by package, then permission. */
    fun listGrants(): List<PackageGrants> =
        GrantList.parseFrom(
            call(
                interfaceId = PermissionAdmin.INTERFACE_ID,
                methodId = PermissionAdmin.LIST_GRANTS,
                payload = ListGrantsRequest.getDefaultInstance().toByteArray(),
            )
        ).packagesList

    /**
     * Changes one permission's grant and returns the state **in effect afterwards**.
     *
     * The return value, not the requested value, goes into the UI: whether a grant really landed is the kernel's call
     * (the package may have just been uninstalled, the permission just downgraded), and the UI should show what is true now.
     *
     * Revoking uses [GrantState.GRANT_STATE_DENIED], not NOT_REQUESTED: the latter means
     * "back to never asked", which isn't a decision the user can make, and the kernel refuses it.
     */
    fun setGrantState(packageId: String, permissionId: String, granted: Boolean): GrantState {
        val target =
            if (granted) GrantState.GRANT_STATE_GRANTED else GrantState.GRANT_STATE_DENIED
        log.info("setGrantState $packageId/$permissionId -> $target")
        return SetGrantStateResponse.parseFrom(
            call(
                interfaceId = PermissionAdmin.INTERFACE_ID,
                methodId = PermissionAdmin.SET_GRANT_STATE,
                payload = SetGrantStateRequest.newBuilder()
                    .setPackageId(packageId)
                    .setPermissionId(permissionId)
                    .setState(target)
                    .build()
                    .toByteArray(),
            )
        ).state
    }
}

fun main() {
    val log = Logger.getLogger("permissionui")
    // componentId must be "main": it matches components.app("main") in the nspkg,
    // and the kernel identifies components by (package_id, component_id)
    val app = PermissionUi(ComponentConfig(componentId = "main"))

    try {
        app.start()
    } catch (e: Exception) {
        log.severe("permissionui: cannot reach control plane: ${e.message}")
        exitProcess(1)
    }

    Runtime.getRuntime().addShutdownHook(Thread { app.close() })

    app.attachComposeDesktop(
        title = WINDOW_TITLE,
        width = 1100.dp,
        height = 760.dp,
        // Back hides the window rather than ending the process.
        //
        // [Not X11WindowControl.hideActiveWindow]: that is an xdotool minimise, which here
        // makes Compose fire onCloseRequest, so one press of Back quits the whole process,
        // the endpoint this component registered dies, and Settings still has it cached.
        //
        // Returning true means "this app handled this Back itself"; don't bubble it further.
        onUnhandledBack = {
            app.window.hide()
            true
        },
        windowHandle = app.window,
        onDisconnect = {
            log.severe("permissionui: control plane lost, exiting")
            exitProcess(1)
        },
    ) {
        PermissionUiScreen(app)
    }
}
