package com.nervus.launcher

import io.github.nervusos.iface.pkgmanager.v1.ListRequest
import io.github.nervusos.iface.pkgmanager.v1.ListResult
import io.github.nervusos.iface.pkgmanager.v1.PackageInfo

/**
 * Method IDs of `nervus.interface.pkg.manager`.
 *
 * The values come from the `PackageManagerMethod` enum in
 * `nervus-ipc/proto/nervus/interface/pkgmanager/v1/pkg_manager.proto`, and **the proto wins**: its `method_meta.method_id`
 * must match the enum numbers (`go/registry`'s ExtractMethodMetas checks this during extraction
 * and fails closed on a mismatch).
 *
 * The constants are written by hand rather than taken from the generated enum because the generated Kotlin
 * gives you `PackageManagerMethod.LIST.number`, which reads worse. If the two ever disagree,
 * the call gets a precise NOT_FOUND; it never silently calls the wrong method.
 */
object PkgManagerMethods {
    const val INTERFACE_ID = "nervus.interface.pkg.manager"

    const val INSTALL = 1
    const val UNINSTALL = 2
    const val LIST = 3
    const val SET_COMPONENT_ENABLED = 4
}

/** Decodes the List result, shared by the desktop and Settings. The payload is protobuf; never hand-roll it */
fun decodeList(payload: ByteArray): List<PackageInfo> =
    ListResult.parseFrom(payload).packagesList

/** LIST takes no arguments, but an empty ListRequest is still sent: the payload type is set by the method */
fun listRequestPayload(): ByteArray = ListRequest.getDefaultInstance().toByteArray()
