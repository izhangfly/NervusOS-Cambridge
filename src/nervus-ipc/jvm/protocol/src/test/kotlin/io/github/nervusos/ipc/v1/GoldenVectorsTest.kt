package io.github.nervusos.ipc.v1

import com.google.protobuf.ByteString
import org.junit.jupiter.api.Assertions.assertArrayEquals
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.DynamicTest
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.TestFactory
import java.io.File

/**
 * JVM-side assertions for the Go↔JVM golden vectors (NRCP §22.6, end of §10.12).
 *
 * Each vector **independently builds** in Kotlin the same message as the Go side, and after `toByteArray()`
 * must equal the committed testdata/golden/<name>.binpb byte for byte. That file is generated on the Go side by
 * `go test ./golden -run TestGoldenUpdate -update`. Both sides assert "the bytes I serialise
 * == the same committed file", so Go and JVM agree byte for byte transitively.
 *
 * The committed .binpb is the single source of truth; this test does **not** write to disk. If the two sides disagree, the schema or
 * one side's construction has drifted, and it must be stopped here.
 */
class GoldenVectorsTest {

    private val goldenDir: File by lazy {
        var dir = File(System.getProperty("user.dir")).absoluteFile
        while (true) {
            if (File(dir, "buf.yaml").exists()) {
                return@lazy File(dir, "testdata/golden")
            }
            dir = dir.parentFile ?: error("repo root (buf.yaml) not found above ${System.getProperty("user.dir")}")
        }
        @Suppress("UNREACHABLE_CODE")
        error("unreachable")
    }

    private fun bytesOf(vararg b: Int): ByteString =
        ByteString.copyFrom(ByteArray(b.size) { b[it].toByte() })

    /** name → the bytes this vector builds on the Kotlin side. One to one with Go's golden.Vectors(). */
    private fun build(name: String): ByteArray = when (name) {
        "response_success_ok" ->
            Envelope.newBuilder().setResponse(
                Response.newBuilder().setRequestId(1).setSuccess(
                    Success.newBuilder()
                        .setCode(StatusCode.STATUS_CODE_OK)
                        .setPayload(ByteString.copyFromUtf8("pong"))
                )
            ).build().toByteArray()

        "response_failure_permission_denied" ->
            Envelope.newBuilder().setResponse(
                Response.newBuilder().setRequestId(2).setFailure(
                    Failure.newBuilder()
                        .setCode(StatusCode.STATUS_CODE_PERMISSION_DENIED)
                        .setPublicMessage("denied")
                )
            ).build().toByteArray()

        "response_failure_unavailable" ->
            Envelope.newBuilder().setResponse(
                Response.newBuilder().setRequestId(3).setFailure(
                    Failure.newBuilder().setCode(StatusCode.STATUS_CODE_UNAVAILABLE)
                )
            ).build().toByteArray()

        "resolve_failure_interface_not_found" ->
            Envelope.newBuilder().setResolveEndpointResult(
                ResolveEndpointResult.newBuilder().setRequestId(4).setFailure(
                    Failure.newBuilder()
                        .setCode(StatusCode.STATUS_CODE_NOT_FOUND)
                        .setErrorDetail(
                            ResolveEndpointErrorDetail.newBuilder()
                                .setReason(ResolveEndpointReason.RESOLVE_ENDPOINT_REASON_INTERFACE_NOT_FOUND)
                                .build().toByteString()
                        )
                )
            ).build().toByteArray()

        "resolve_failure_version_mismatch" ->
            Envelope.newBuilder().setResolveEndpointResult(
                ResolveEndpointResult.newBuilder().setRequestId(5).setFailure(
                    Failure.newBuilder()
                        .setCode(StatusCode.STATUS_CODE_FAILED_PRECONDITION)
                        .setErrorDetail(
                            ResolveEndpointErrorDetail.newBuilder()
                                .setReason(ResolveEndpointReason.RESOLVE_ENDPOINT_REASON_VERSION_MISMATCH)
                                .build().toByteString()
                        )
                )
            ).build().toByteArray()

        "resolve_failure_unknown_reason" ->
            Envelope.newBuilder().setResolveEndpointResult(
                ResolveEndpointResult.newBuilder().setRequestId(6).setFailure(
                    Failure.newBuilder()
                        .setCode(StatusCode.STATUS_CODE_NOT_FOUND)
                        .setErrorDetail(
                            // reason=99: an unknown enum value, kept as the raw integer with setReasonValue
                            ResolveEndpointErrorDetail.newBuilder()
                                .setReasonValue(99)
                                .build().toByteString()
                        )
                )
            ).build().toByteArray()

        "failure_malformed_detail_internal" ->
            Envelope.newBuilder().setResponse(
                Response.newBuilder().setRequestId(7).setFailure(
                    Failure.newBuilder()
                        .setCode(StatusCode.STATUS_CODE_INTERNAL)
                        .setErrorDetail(bytesOf(0xFF, 0xFF, 0xFF, 0xFF, 0x0F))
                )
            ).build().toByteArray()

        "acquire_control_human_base" ->
            Envelope.newBuilder().setAcquireControl(
                AcquireControl.newBuilder()
                    .setRequestId(8)
                    .setControllerClass(ControllerClass.CONTROLLER_CLASS_HUMAN)
                    .setResource(
                        ResourceSelector.newBuilder()
                            .setType("nervus.resource.motion.base").setRole("main")
                    )
                    .setRequestedDeadlineNanos(500_000_000)
            ).build().toByteArray()

        "acquire_control_ai_arm" ->
            Envelope.newBuilder().setAcquireControl(
                AcquireControl.newBuilder()
                    .setRequestId(9)
                    .setControllerClass(ControllerClass.CONTROLLER_CLASS_AI)
                    .setResource(
                        ResourceSelector.newBuilder()
                            .setType("nervus.resource.manipulator").setRole("main")
                    )
                    .setRequestedDeadlineNanos(1_000_000_000)
            ).build().toByteArray()

        "acquire_control_result_success" ->
            Envelope.newBuilder().setAcquireControlResult(
                AcquireControlResult.newBuilder().setRequestId(10).setSuccess(
                    AcquireControlSuccess.newBuilder()
                        .setLeaseId(0xABCD)
                        .setMotionEpoch(3)
                        .setDeadlineNanos(1_234_567_890)
                        .setResourceHandle("base.main")
                )
            ).build().toByteArray()

        "acquire_control_result_held_by_human" ->
            Envelope.newBuilder().setAcquireControlResult(
                AcquireControlResult.newBuilder().setRequestId(11).setFailure(
                    Failure.newBuilder()
                        .setCode(StatusCode.STATUS_CODE_FAILED_PRECONDITION)
                        .setErrorDetail(
                            ControlLeaseErrorDetail.newBuilder()
                                .setReason(ControlLeaseErrorReason.CONTROL_LEASE_ERROR_REASON_HELD_BY_HUMAN)
                                .build().toByteString()
                        )
                )
            ).build().toByteArray()

        "acquire_control_result_unknown_reason" ->
            Envelope.newBuilder().setAcquireControlResult(
                AcquireControlResult.newBuilder().setRequestId(12).setFailure(
                    Failure.newBuilder()
                        .setCode(StatusCode.STATUS_CODE_FAILED_PRECONDITION)
                        .setErrorDetail(
                            ControlLeaseErrorDetail.newBuilder()
                                .setReasonValue(99)
                                .build().toByteString()
                        )
                )
            ).build().toByteArray()

        "release_control" ->
            Envelope.newBuilder().setReleaseControl(
                ReleaseControl.newBuilder().setRequestId(13).setLeaseId(0xABCD)
            ).build().toByteArray()

        "release_control_result_success" ->
            Envelope.newBuilder().setReleaseControlResult(
                ReleaseControlResult.newBuilder().setRequestId(14).setSuccess(
                    Success.newBuilder().setCode(StatusCode.STATUS_CODE_OK)
                )
            ).build().toByteArray()

        "dispatch_control_execution_context" ->
            Envelope.newBuilder()
                .setProtocolMajor(1)
                .setProtocolMinor(1)
                .setDispatch(
                    Dispatch.newBuilder()
                        .setRouteId(15)
                        .setEndpointId(23)
                        .setMethodId(7)
                        .setRemainingMs(250)
                        .setPayload(ByteString.copyFromUtf8("move"))
                        .setExecutionContext(
                            ExecutionContext.newBuilder()
                                .setLeaseId(0xABCD)
                                .setControllerClass(ControllerClass.CONTROLLER_CLASS_HUMAN)
                                .setMotionEpoch(3)
                                .setDeadlineNanos(1_234_567_890)
                                .setCommandSequence(17)
                                .setResourceHandle("base.main")
                                .setResourceGeneration(4)
                        )
                ).build().toByteArray()

        "interface_schema_bundle" ->
            InterfaceSchemaBundle.newBuilder()
                .setInterfaceId("com.acme.interface.gripper")
                .setVersion(1)
                .setSchemaHash(bytesOf(0xDE, 0xAD, 0xBE, 0xEF))
                .setFileDescriptorSet(bytesOf(0x0A, 0x03, 'a'.code, 'b'.code, 'c'.code))
                .setMethodEnumType("com.acme.gripper.v1.GripperMethod")
                .build().toByteArray()

        "interface_schema_bundle_set" ->
            InterfaceSchemaBundleSet.newBuilder()
                .addBundles(
                    InterfaceSchemaBundle.newBuilder()
                        .setInterfaceId("nervus.interface.motion.base")
                        .setVersion(1)
                        .setSchemaHash(bytesOf(0x01, 0x02, 0x03, 0x04))
                        .setFileDescriptorSet(bytesOf(0x0A, 0x01, 'x'.code))
                        .setMethodEnumType("nervus.interface.motion.v1.BaseMotionMethod")
                )
                .addBundles(
                    InterfaceSchemaBundle.newBuilder()
                        .setInterfaceId("com.acme.interface.gripper")
                        .setVersion(2)
                        .setSchemaHash(bytesOf(0xDE, 0xAD, 0xBE, 0xEF))
                        .setFileDescriptorSet(bytesOf(0x0A, 0x01, 'y'.code))
                        .setMethodEnumType("com.acme.gripper.v2.GripperMethod")
                )
                .build().toByteArray()

        "transfer_handle_peer" ->
            TransferHandle.newBuilder()
                .setTransferId(bytesOf(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15))
                .setAttachTicket(ByteString.copyFromUtf8("0123456789abcdef0123456789abcdef"))
                .setRole(TransferRole.TRANSFER_ROLE_PEER)
                .setMode(TransferMode.TRANSFER_MODE_FRAMED_RELAY)
                .setExpiresAtMonotonicNanos(9_876_543_210)
                .setDataPlaneEndpoint("/run/nervus/nervud-transfer.sock")
                .build().toByteArray()

        "attach_transfer_result_success" ->
            AttachTransferResult.newBuilder()
                .setSuccess(
                    AttachTransferSuccess.newBuilder()
                        .setMode(TransferMode.TRANSFER_MODE_FRAMED_RELAY)
                        .setMaxPacketBytes(1_048_576)
                        .setMaxBytesPerSecond(8_388_608)
                )
                .build().toByteArray()

        else -> error("unknown golden vector: $name")
    }

    /** Extracts every vector name from vectors.json (no third-party JSON library; scans the "name" fields by hand). */
    private fun vectorNames(): List<String> {
        val text = File(goldenDir, "vectors.json").readText()
        return Regex("\"name\"\\s*:\\s*\"([^\"]+)\"").findAll(text).map { it.groupValues[1] }.toList()
    }

    @TestFactory
    fun `each vector serializes byte-identically to committed golden`(): List<DynamicTest> {
        val names = vectorNames()
        assertTrue(names.isNotEmpty(), "vectors.json has no vectors (run Go -update first)")
        return names.map { name ->
            DynamicTest.dynamicTest(name) {
                val want = File(goldenDir, "$name.binpb").readBytes()
                val got = build(name)
                assertArrayEquals(
                    want, got,
                    "JVM bytes for '$name' differ from committed golden .binpb"
                )
            }
        }
    }

    @Test
    fun `unknown reason keeps generic code and round-trips`() {
        val env = Envelope.parseFrom(File(goldenDir, "resolve_failure_unknown_reason.binpb").readBytes())
        val f = env.resolveEndpointResult.failure
        assertEquals(StatusCode.STATUS_CODE_NOT_FOUND, f.code, "generic code must survive unknown reason")
        val d = ResolveEndpointErrorDetail.parseFrom(f.errorDetail)
        assertEquals(99, d.reasonValue, "unknown reason must round-trip as raw 99")
        assertEquals(ResolveEndpointReason.UNRECOGNIZED, d.reason, "unknown enum surfaces as UNRECOGNIZED")
    }

    @Test
    fun `malformed detail carries INTERNAL`() {
        val env = Envelope.parseFrom(File(goldenDir, "failure_malformed_detail_internal.binpb").readBytes())
        assertEquals(StatusCode.STATUS_CODE_INTERNAL, env.response.failure.code)
    }

    @Test
    fun `acquire control decodes resource-scoped fields`() {
        val env = Envelope.parseFrom(File(goldenDir, "acquire_control_human_base.binpb").readBytes())
        val a = env.acquireControl
        assertEquals(ControllerClass.CONTROLLER_CLASS_HUMAN, a.controllerClass)
        assertEquals("nervus.resource.motion.base", a.resource.type)
        assertEquals("main", a.resource.role)
        assertEquals(500_000_000, a.requestedDeadlineNanos)
    }

    @Test
    fun `held-by-human is a distinguishable typed reason`() {
        val env = Envelope.parseFrom(File(goldenDir, "acquire_control_result_held_by_human.binpb").readBytes())
        val f = env.acquireControlResult.failure
        assertEquals(StatusCode.STATUS_CODE_FAILED_PRECONDITION, f.code)
        val d = ControlLeaseErrorDetail.parseFrom(f.errorDetail)
        assertEquals(ControlLeaseErrorReason.CONTROL_LEASE_ERROR_REASON_HELD_BY_HUMAN, d.reason)
    }

    @Test
    fun `dispatch execution context preserves kernel proof`() {
        val env = Envelope.parseFrom(File(goldenDir, "dispatch_control_execution_context.binpb").readBytes())
        val context = env.dispatch.executionContext
        assertEquals(1, env.protocolMinor)
        assertEquals(15, env.dispatch.routeId)
        assertEquals(0xABCD, context.leaseId)
        assertEquals(ControllerClass.CONTROLLER_CLASS_HUMAN, context.controllerClass)
        assertEquals(3, context.motionEpoch)
        assertEquals(1_234_567_890, context.deadlineNanos)
        assertEquals(17, context.commandSequence)
        assertEquals("base.main", context.resourceHandle)
        assertEquals(4, context.resourceGeneration)
    }

    @Test
    fun `schema bundle decodes id and version`() {
        val b = InterfaceSchemaBundle.parseFrom(File(goldenDir, "interface_schema_bundle.binpb").readBytes())
        assertEquals("com.acme.interface.gripper", b.interfaceId)
        assertEquals(1, b.version)
        assertEquals("com.acme.gripper.v1.GripperMethod", b.methodEnumType)
    }

    @Test
    fun `transfer handle preserves ticket role and mode`() {
        val h = TransferHandle.parseFrom(File(goldenDir, "transfer_handle_peer.binpb").readBytes())
        assertEquals(16, h.transferId.size())
        assertEquals(32, h.attachTicket.size())
        assertEquals(TransferRole.TRANSFER_ROLE_PEER, h.role)
        assertEquals(TransferMode.TRANSFER_MODE_FRAMED_RELAY, h.mode)
    }
}
