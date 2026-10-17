// Package contract is the single runtime source for the Gazebo Provider's
// Nervus identities and interface schema hashes.
package contract

import (
	"fmt"

	simulationv1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/provider/v1"
	"github.com/nervus-os/nervus-gazebo-provider/internal/motion"
	"github.com/nervus-os/nervus-gazebo-provider/internal/motionprofile"
	basemotionv1 "github.com/nervus-os/nervus-ipc/protocol/interface/basemotionv1"
	manipulatorv1 "github.com/nervus-os/nervus-ipc/protocol/interface/manipulatorv1"
	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"
)

const (
	PackageID   = "gazebo.simulation.motion.provider"
	ComponentID = "main"

	ManagerInterface = "gazebo.simulation.motion.provider.manager"
	ControlInterface = "gazebo.simulation.motion.provider.control"
	InterfaceMajor   = 1

	WorldResourceType = "gazebo.simulation.motion.provider.resource.world"
	WorldResourceRole = "simulation.world.main"
	WorldKindLabel    = "gazebo.simulation.motion.provider.kind"

	QueryPermission     = "gazebo.simulation.motion.provider.permission.query"
	LifecyclePermission = "gazebo.simulation.motion.provider.permission.lifecycle"
	ControlPermission   = "gazebo.simulation.motion.provider.permission.control"

	BaseInterface        = motion.InterfaceBaseMotion
	ManipulatorInterface = motion.InterfaceManipulator
	BaseResourceType     = "nervus.resource.motion.base"
	ArmResourceType      = "nervus.resource.manipulator.arm"
	BaseResourceRole     = "base.main"
	LeftArmResourceRole  = "arm.left"
	RightArmResourceRole = "arm.right"
)

// SchemaBundles derives both interface contracts from generated protobuf
// descriptors. The Provider artifact generator and runtime registration call
// this same function, so a stale hard-coded runtime hash cannot be introduced.
func SchemaBundles() (manager, control *ipcv1.InterfaceSchemaBundle, err error) {
	manager, err = ipcregistry.BuildSchemaBundleWithEvents(
		ManagerInterface,
		InterfaceMajor,
		simulationv1.ManagerMethod(0).Descriptor(),
		simulationv1.ManagerEvent(0).Descriptor(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("build %s schema bundle: %w", ManagerInterface, err)
	}

	control, err = ipcregistry.BuildSchemaBundleWithEvents(
		ControlInterface,
		InterfaceMajor,
		simulationv1.ControlMethod(0).Descriptor(),
		simulationv1.ControlEvent(0).Descriptor(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("build %s schema bundle: %w", ControlInterface, err)
	}
	return manager, control, nil
}

func StandardSchemaBundles() (base, manipulator *ipcv1.InterfaceSchemaBundle, err error) {
	base, err = ipcregistry.BuildSchemaBundle(
		BaseInterface,
		InterfaceMajor,
		basemotionv1.BaseMotionMethod(0).Descriptor(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("build %s schema bundle: %w", BaseInterface, err)
	}
	manipulator, err = ipcregistry.BuildSchemaBundle(
		ManipulatorInterface,
		InterfaceMajor,
		manipulatorv1.ManipulatorMethod(0).Descriptor(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("build %s schema bundle: %w", ManipulatorInterface, err)
	}
	return base, manipulator, nil
}

func StandardSchemaHashes() (base, manipulator []byte, err error) {
	baseBundle, manipulatorBundle, err := StandardSchemaBundles()
	if err != nil {
		return nil, nil, err
	}
	return append([]byte(nil), baseBundle.GetSchemaHash()...),
		append([]byte(nil), manipulatorBundle.GetSchemaHash()...), nil
}

func MotionProfileAllowlist() motionprofile.Allowlist {
	return motionprofile.Allowlist{Slots: map[string]motionprofile.Slot{
		WorldResourceRole: {
			ResourceType: WorldResourceType,
			InterfaceID:  ControlInterface,
			Major:        InterfaceMajor,
		},
		BaseResourceRole: {
			ResourceType: BaseResourceType,
			InterfaceID:  BaseInterface,
			Major:        InterfaceMajor,
		},
		LeftArmResourceRole: {
			ResourceType: ArmResourceType,
			InterfaceID:  ManipulatorInterface,
			Major:        InterfaceMajor,
		},
		RightArmResourceRole: {
			ResourceType: ArmResourceType,
			InterfaceID:  ManipulatorInterface,
			Major:        InterfaceMajor,
		},
	}}
}

// SchemaHashes returns owned copies suitable for sdk.RegisterRequest.
func SchemaHashes() (manager, control []byte, err error) {
	managerBundle, controlBundle, err := SchemaBundles()
	if err != nil {
		return nil, nil, err
	}
	return append([]byte(nil), managerBundle.GetSchemaHash()...),
		append([]byte(nil), controlBundle.GetSchemaHash()...), nil
}
