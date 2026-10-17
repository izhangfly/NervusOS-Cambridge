// Package motion contains the compiled semantic adapter registry and reusable
// motion-control primitives. The registry is the authority for Gazebo topic
// direction and protobuf types; Profile JSON may only select these entries.
package motion

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	profilev1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/motion/profile/v1"
)

const (
	InterfaceCompatibility = "gazebo.simulation.motion.provider.control"
	InterfaceBaseMotion    = "nervus.interface.motion.base"
	InterfaceManipulator   = "nervus.interface.manipulator.arm"

	TypeDouble          = "gz.msgs.Double"
	TypeModel           = "gz.msgs.Model"
	TypeJointTrajectory = "gz.msgs.JointTrajectory"
	TypeTwist           = "gz.msgs.Twist"
	TypeOdometry        = "gz.msgs.Odometry"
	TypePose            = "gz.msgs.Pose"
)

type Direction uint8

const (
	DirectionUnspecified Direction = iota
	DirectionPublish
	DirectionRequest
)

type Spec struct {
	Kind             profilev1.AdapterKind
	Name             string
	Direction        Direction
	CommandType      string
	StateTypes       map[profilev1.StateSourceKind]string
	Semantics        map[profilev1.CapabilitySemantic]bool
	CommandTokens    map[string]bool
	StateTokens      map[string]bool
	RuntimeAvailable bool
}

var registry = map[profilev1.AdapterKind]Spec{
	profilev1.AdapterKind_ADAPTER_KIND_JOINT_POSITION: {
		Kind:        profilev1.AdapterKind_ADAPTER_KIND_JOINT_POSITION,
		Name:        "joint_position",
		Direction:   DirectionPublish,
		CommandType: TypeDouble,
		StateTypes: map[profilev1.StateSourceKind]string{
			profilev1.StateSourceKind_STATE_SOURCE_KIND_JOINT_STATE: TypeModel,
		},
		Semantics: map[profilev1.CapabilitySemantic]bool{
			profilev1.CapabilitySemantic_CAPABILITY_SEMANTIC_COMPATIBILITY_JOINT_CONTROL: true,
		},
		CommandTokens:    tokenSet("model", "joint", "axis"),
		StateTokens:      tokenSet("world", "model"),
		RuntimeAvailable: true,
	},
	profilev1.AdapterKind_ADAPTER_KIND_JOINT_TRAJECTORY: {
		Kind:        profilev1.AdapterKind_ADAPTER_KIND_JOINT_TRAJECTORY,
		Name:        "joint_trajectory",
		Direction:   DirectionPublish,
		CommandType: TypeJointTrajectory,
		StateTypes: map[profilev1.StateSourceKind]string{
			profilev1.StateSourceKind_STATE_SOURCE_KIND_JOINT_STATE: TypeModel,
		},
		Semantics: map[profilev1.CapabilitySemantic]bool{
			profilev1.CapabilitySemantic_CAPABILITY_SEMANTIC_MANIPULATOR_ARM: true,
		},
		CommandTokens: tokenSet("model"),
		StateTokens:   tokenSet("world", "model"),
	},
	profilev1.AdapterKind_ADAPTER_KIND_DIFF_DRIVE: {
		Kind:        profilev1.AdapterKind_ADAPTER_KIND_DIFF_DRIVE,
		Name:        "diff_drive",
		Direction:   DirectionPublish,
		CommandType: TypeTwist,
		StateTypes: map[profilev1.StateSourceKind]string{
			profilev1.StateSourceKind_STATE_SOURCE_KIND_ODOMETRY:  TypeOdometry,
			profilev1.StateSourceKind_STATE_SOURCE_KIND_ROOT_POSE: TypePose,
		},
		Semantics: map[profilev1.CapabilitySemantic]bool{
			profilev1.CapabilitySemantic_CAPABILITY_SEMANTIC_BASE_MOTION: true,
		},
		CommandTokens: tokenSet("model"),
		StateTokens:   tokenSet("world", "model"),
	},
	profilev1.AdapterKind_ADAPTER_KIND_HUMANOID_GAIT: {
		Kind:        profilev1.AdapterKind_ADAPTER_KIND_HUMANOID_GAIT,
		Name:        "humanoid_gait",
		Direction:   DirectionPublish,
		CommandType: TypeTwist,
		StateTypes: map[profilev1.StateSourceKind]string{
			profilev1.StateSourceKind_STATE_SOURCE_KIND_ROOT_POSE: TypePose,
			profilev1.StateSourceKind_STATE_SOURCE_KIND_ODOMETRY:  TypeOdometry,
		},
		Semantics: map[profilev1.CapabilitySemantic]bool{
			profilev1.CapabilitySemantic_CAPABILITY_SEMANTIC_BASE_MOTION: true,
		},
		CommandTokens: tokenSet("model"),
		StateTokens:   tokenSet("world", "model"),
	},
}

func Lookup(kind profilev1.AdapterKind) (Spec, bool) {
	spec, ok := registry[kind]
	return spec, ok
}

func Specs() []Spec {
	out := make([]Spec, 0, len(registry))
	for _, spec := range registry {
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

func ExpectedInterface(semantic profilev1.CapabilitySemantic) string {
	switch semantic {
	case profilev1.CapabilitySemantic_CAPABILITY_SEMANTIC_COMPATIBILITY_JOINT_CONTROL:
		return InterfaceCompatibility
	case profilev1.CapabilitySemantic_CAPABILITY_SEMANTIC_BASE_MOTION:
		return InterfaceBaseMotion
	case profilev1.CapabilitySemantic_CAPABILITY_SEMANTIC_MANIPULATOR_ARM:
		return InterfaceManipulator
	default:
		return ""
	}
}

var templateToken = regexp.MustCompile(`\{([a-z][a-z0-9_]*)\}`)

func ValidateTemplate(value string, allowed map[string]bool) error {
	if value == "" || len(value) > 512 || !strings.HasPrefix(value, "/") ||
		strings.Contains(value, "..") || strings.ContainsAny(value, "\x00\r\n\t ") {
		return fmt.Errorf("motion: unsafe Gazebo name template")
	}
	remaining := templateToken.ReplaceAllStringFunc(value, func(token string) string {
		name := token[1 : len(token)-1]
		if !allowed[name] {
			return "{!invalid!}"
		}
		return "x"
	})
	if strings.Contains(remaining, "{") || strings.Contains(remaining, "}") ||
		strings.Contains(remaining, "//") {
		return fmt.Errorf("motion: unsupported or malformed template token")
	}
	return nil
}

func ExpandTemplate(value string, fields map[string]string) (string, error) {
	result := templateToken.ReplaceAllStringFunc(value, func(token string) string {
		return fields[token[1:len(token)-1]]
	})
	if strings.Contains(result, "{") || strings.Contains(result, "}") {
		return "", fmt.Errorf("motion: missing template value")
	}
	if err := ValidateExpandedName(result); err != nil {
		return "", err
	}
	return result, nil
}

func ValidateExpandedName(value string) error {
	if value == "" || len(value) > 1024 || !strings.HasPrefix(value, "/") ||
		strings.Contains(value, "..") || strings.Contains(value, "//") ||
		strings.ContainsAny(value, "\x00\r\n\t {}") {
		return fmt.Errorf("motion: unsafe expanded Gazebo name")
	}
	return nil
}

func tokenSet(values ...string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}
