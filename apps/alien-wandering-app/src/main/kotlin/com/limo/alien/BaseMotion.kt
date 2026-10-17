package com.limo.alien

import com.nervus.sdk.annotations.Interface
import com.nervus.sdk.annotations.Method
import io.github.nervusos.iface.basemotion.v1.GetMotionStateRequest
import io.github.nervusos.iface.basemotion.v1.MotionState
import io.github.nervusos.iface.basemotion.v1.SetVelocityRequest
import io.github.nervusos.iface.basemotion.v1.StopRequest

/**
 * Kotlin binding for the standard NervusOS interface **BaseMotion@1**
 * (`nervus.interface.motion.base`). The SDK's InterfaceProxy marshals each `@Method`
 * request/response as protobuf over Port A → nervud → DimOS → the Limo base.
 *
 * Units (body frame): linear_x m/s (fwd+), linear_y m/s (left+; ignored on diff/ackermann),
 * angular_z rad/s (CCW+).
 */
@Interface(id = "nervus.interface.motion.base")
interface BaseMotion {
    @Method(id = 1) suspend fun setVelocity(req: SetVelocityRequest)          // fire-and-forget
    @Method(id = 2) suspend fun stop(req: StopRequest)
    @Method(id = 3) suspend fun getMotionState(req: GetMotionStateRequest): MotionState
}
