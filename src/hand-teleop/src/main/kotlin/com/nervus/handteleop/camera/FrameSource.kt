package com.nervus.handteleop.camera

import com.nervus.handteleop.vision.RgbFrame

/**
 * Where frames come from: the camera service through NervusOS ([CameraSource]), or a V4L2 device read
 * directly when the app runs on its own ([V4l2Source]). Frames are as the camera sees them, never
 * mirrored: the body pose tells left from right on the real image, and the UI mirrors for display.
 */
interface FrameSource : AutoCloseable {
    fun start()
    fun latest(): RgbFrame?
    val fps: Float
    val format: String
    /** Why there are no frames, in words for the operator; null while frames flow. */
    val error: String?
    /** What the UI says while it waits for the first frame. */
    val waiting: String
}
