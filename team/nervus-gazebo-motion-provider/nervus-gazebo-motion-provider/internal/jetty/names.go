package jetty

import "fmt"

const (
	typeBoolean         = "gz.msgs.Boolean"
	typeDouble          = "gz.msgs.Double"
	typeTwist           = "gz.msgs.Twist"
	typeOdometry        = "gz.msgs.Odometry"
	typePose            = "gz.msgs.Pose"
	typeEmpty           = "gz.msgs.Empty"
	typeModel           = "gz.msgs.Model"
	typeScene           = "gz.msgs.Scene"
	typeWorldControl    = "gz.msgs.WorldControl"
	typeWorldStatistics = "gz.msgs.WorldStatistics"
)

func controlService(world string) string {
	return fmt.Sprintf("/world/%s/control", world)
}

func sceneService(world string) string {
	return fmt.Sprintf("/world/%s/scene/info", world)
}

func statsTopic(world string) string {
	return fmt.Sprintf("/world/%s/stats", world)
}

func jointStateTopic(world, model string) string {
	return fmt.Sprintf("/world/%s/model/%s/joint_state", world, model)
}

func jointCommandTopic(model, joint string, axis uint32) string {
	return fmt.Sprintf("/model/%s/joint/%s/%d/cmd_pos", model, joint, axis)
}

func jointKey(model, joint string, axis uint32) string {
	return fmt.Sprintf("%s\x00%s\x00%d", model, joint, axis)
}

func baseCommandTopic(model string) string {
	return fmt.Sprintf("/model/%s/cmd_vel", model)
}
