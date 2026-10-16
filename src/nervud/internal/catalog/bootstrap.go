package catalog

import (
	"fmt"

	basemotionv1 "github.com/nervus-os/nervus-ipc/protocol/interface/basemotionv1"
	manipulatorv1 "github.com/nervus-os/nervus-ipc/protocol/interface/manipulatorv1"
	operationv1 "github.com/nervus-os/nervus-ipc/protocol/interface/operationv1"
	permissionv1 "github.com/nervus-os/nervus-ipc/protocol/interface/permissionv1"
	pkgmanagerv1 "github.com/nervus-os/nervus-ipc/protocol/interface/pkgmanagerv1"
	resourcedirv1 "github.com/nervus-os/nervus-ipc/protocol/interface/resourcedirv1"
	safetyv1 "github.com/nervus-os/nervus-ipc/protocol/interface/safetyv1"
	transferv1 "github.com/nervus-os/nervus-ipc/protocol/interface/transferv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"
	"google.golang.org/protobuf/proto"

	"github.com/nervus-os/nervud/internal/identity"
)

// DefaultBootstrap returns kernel-owned definitions that must exist before any
// package can register or consume a provider. Capability-specific additions
// belong in signed ProviderArtifacts, not in this function.
func DefaultBootstrap() ([]Source, error) {
	artifacts, err := buildBootstrapArtifacts()
	if err != nil {
		return nil, err
	}
	kernelSigner := SignerEvidence{
		Roles: []string{rolePlatformRelease},
		VerifiedSigners: []VerifiedSigner{{
			Role: rolePlatformRelease, KeyID: "nervus-kernel-builtin-v1",
		}},
	}
	return []Source{{
		PackageID: KernelPackageID,
		Kind:      SourceKindKernel,
		Trust:     identity.TrustPlatform,
		Signers:   kernelSigner,
		Exports: []ExportBinding{
			{ComponentID: "builtin.safety", InterfaceID: InterfaceSafetyControl},
			{ComponentID: "builtin.transfer", InterfaceID: InterfaceTransferControl},
			{ComponentID: "builtin.resourcedir", InterfaceID: InterfaceResourceDirectory},
			{ComponentID: "builtin.operation", InterfaceID: InterfaceOperationControl},
			{ComponentID: "builtin.permission", InterfaceID: InterfacePermissionAdmin},
			// The self-check surface and the grant surface belong to the same built-in component: both read the same state
			// (permission.Registry), and a component is the unit of "who implements it", not of "how high the bar is".
			// The bars are in each bootstrapInterface and differ a lot (one needs
			// perm.permission.admin, the other has none), but the implementer really is the same code.
			{ComponentID: "builtin.permission", InterfaceID: InterfacePermissionSelf},
		},
		Artifacts: artifacts,
		KernelBuiltins: []KernelBuiltin{{
			ComponentID:        "builtin.power",
			InterfaceID:        InterfacePower,
			Major:              1,
			RequiredPermission: "perm.authority.power",
			Methods: []*ipcv1.MethodMeta{
				{
					MethodId:           1,
					RequiredPermission: "perm.authority.power",
					RiskClass:          ipcv1.RiskClass_RISK_CLASS_CRITICAL_SAFETY,
				},
				{
					MethodId:           2,
					RequiredPermission: "perm.authority.power",
					RiskClass:          ipcv1.RiskClass_RISK_CLASS_CRITICAL_SAFETY,
				},
			},
		}},
	}}, nil
}

// BootstrapSources is the explicit construction entry used by main and tests.
func BootstrapSources() ([]Source, error) {
	return DefaultBootstrap()
}

func buildBootstrapArtifacts() (*ipcregistry.ProviderArtifacts, error) {
	baseBundle, err := ipcregistry.BuildSchemaBundle(
		InterfaceMotionBase, 1, basemotionv1.BaseMotionMethod(0).Descriptor())
	if err != nil {
		return nil, fmt.Errorf("catalog: build base-motion bootstrap schema: %w", err)
	}
	manipulatorBundle, err := ipcregistry.BuildSchemaBundle(
		InterfaceManipulatorArm, 1, manipulatorv1.ManipulatorMethod(0).Descriptor())
	if err != nil {
		return nil, fmt.Errorf("catalog: build manipulator bootstrap schema: %w", err)
	}
	packageBundle, err := ipcregistry.BuildSchemaBundle(
		InterfacePackageManager, 1, pkgmanagerv1.PackageManagerMethod(0).Descriptor())
	if err != nil {
		return nil, fmt.Errorf("catalog: build package-manager bootstrap schema: %w", err)
	}
	safetyBundle, err := ipcregistry.BuildSchemaBundle(
		InterfaceSafetyControl, 1, safetyv1.SafetyControlMethod(0).Descriptor())
	if err != nil {
		return nil, fmt.Errorf("catalog: build safety bootstrap schema: %w", err)
	}
	transferBundle, err := ipcregistry.BuildSchemaBundle(
		InterfaceTransferControl, 1, transferv1.TransferControlMethod(0).Descriptor())
	if err != nil {
		return nil, fmt.Errorf("catalog: build transfer bootstrap schema: %w", err)
	}
	resourceDirBundle, err := ipcregistry.BuildSchemaBundle(
		InterfaceResourceDirectory, 1, resourcedirv1.ResourceDirectoryMethod(0).Descriptor())
	if err != nil {
		return nil, fmt.Errorf("catalog: build resource-directory bootstrap schema: %w", err)
	}
	permissionBundle, err := ipcregistry.BuildSchemaBundle(
		InterfacePermissionAdmin, 1, permissionv1.PermissionAdminMethod(0).Descriptor())
	if err != nil {
		return nil, fmt.Errorf("catalog: build permission-admin bootstrap schema: %w", err)
	}
	permissionUIBundle, err := ipcregistry.BuildSchemaBundle(
		InterfacePermissionUI, 1, permissionv1.PermissionUiMethod(0).Descriptor())
	if err != nil {
		return nil, fmt.Errorf("catalog: build permission-ui bootstrap schema: %w", err)
	}
	permissionSelfBundle, err := ipcregistry.BuildSchemaBundle(
		InterfacePermissionSelf, 1, permissionv1.PermissionSelfMethod(0).Descriptor())
	if err != nil {
		return nil, fmt.Errorf("catalog: build permission-self bootstrap schema: %w", err)
	}
	// Has an event enum: OperationChanged carries a payload, so it must go through a bundle rather than being inlined in the
	// descriptor. The inline path is for metadata interfaces and doesn't allow payload_type.
	operationBundle, err := ipcregistry.BuildSchemaBundleWithEvents(
		InterfaceOperationControl, 1,
		operationv1.OperationControlMethod(0).Descriptor(),
		operationv1.OperationControlEvent(0).Descriptor())
	if err != nil {
		return nil, fmt.Errorf("catalog: build operation-control bootstrap schema: %w", err)
	}

	descriptor := &ipcv1.ProviderDescriptor{
		PackageId: KernelPackageID,
		Interfaces: []*ipcv1.ProvidedInterface{
			bootstrapInterface(
				InterfaceMotionBase,
				baseBundle,
				"perm.motion.control",
				ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
				[]string{ResourceMotionBase},
				ResourceMotionBase,
				"base.main",
			),
			bootstrapInterface(
				InterfaceManipulatorArm,
				manipulatorBundle,
				"perm.manipulator.control",
				ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
				[]string{ResourceManipulatorArm},
				ResourceManipulatorArm,
				"arm.main",
			),
			bootstrapInterface(
				InterfacePackageManager,
				packageBundle,
				"perm.pkg.query",
				ipcv1.RiskClass_RISK_CLASS_UNSPECIFIED,
				nil,
				"",
				"",
			),
			bootstrapInterface(
				InterfaceSafetyControl,
				safetyBundle,
				"perm.safety.observe",
				ipcv1.RiskClass_RISK_CLASS_UNSPECIFIED,
				nil,
				"",
				"",
			),
			bootstrapInterface(
				InterfaceTransferControl,
				transferBundle,
				"",
				ipcv1.RiskClass_RISK_CLASS_UNSPECIFIED,
				nil,
				"",
				"",
			),
			// The resource directory binds no resource: what it describes is the resources themselves.
			// Binding one would make "list every camera" first require resolving to one particular camera.
			bootstrapInterface(
				InterfaceResourceDirectory,
				resourceDirBundle,
				"perm.resource.query",
				ipcv1.RiskClass_RISK_CLASS_UNSPECIFIED,
				nil,
				"",
				"",
			),
			// The grant surface binds no resource either: it manages permissions, not devices.
			//
			// required_permission is perm.permission.admin itself: the interface bar
			// and the method bar are the same, so a caller without it can't even Resolve,
			// and never reaches the half-open state of "can resolve but every method is refused".
			bootstrapInterface(
				InterfacePermissionAdmin,
				permissionBundle,
				"perm.permission.admin",
				ipcv1.RiskClass_RISK_CLASS_UNSPECIFIED,
				nil,
				"",
				"",
			),
			// Confirmation screens. Unlike the one above, the kernel does not implement this interface. It's listed here
			// only to pin down the contract and the bar; the implementer is nervus.permissionui, the same shape as
			// InterfacePackageManager (implemented by pkgmanagerd). So it
			// doesn't appear in Exports, and the kernel registers no handler for it.
			//
			// required_permission is perm.pkg.query rather than perm.pkg.install:
			// the interface bar should only govern "who can resolve the confirmation screen", and each method's own bar is stricter
			//  (ConfirmInstall needs perm.pkg.install). If the interface bar were
			// install, a caller that only wants to open the permission management page would first have to request install permission.
			bootstrapInterface(
				InterfacePermissionUI,
				permissionUIBundle,
				"perm.pkg.query",
				ipcv1.RiskClass_RISK_CLASS_UNSPECIFIED,
				nil,
				"",
				"",
			),
			// The self-check surface. Implemented in the kernel (the same Module as .admin), so it appears in
			// Exports.
			//
			// No required_permission. Same shape as operation.control, but for a different
			// reason: that one is constrained by ownership, this one by "the target is always the caller itself"
			//  (the request has no package_id; the identity comes from BuiltinCall.Caller). So what's let through with no bar
			// is only "check myself", not "check any package".
			//
			// Adding a permission would do harm: if it were USER_CONSENT too, it would create a loop of "you need
			// the permission to check whether you have permissions"; if it were NORMAL, everyone would have it,
			// and it would only add a line of noise to every app's manifest.
			bootstrapInterface(
				InterfacePermissionSelf,
				permissionSelfBundle,
				"",
				ipcv1.RiskClass_RISK_CLASS_UNSPECIFIED,
				nil,
				"",
				"",
			),
			// No required_permission: whether someone may query an operation is decided by its
			// own ownership (Manager.Get's canSee), not by a global
			// permission. Adding one would only make "holding it lets you see every operation on the machine"
			// possible.
			bootstrapInterface(
				InterfaceOperationControl,
				operationBundle,
				"",
				ipcv1.RiskClass_RISK_CLASS_UNSPECIFIED,
				nil,
				"",
				"",
			),
		},
		Resources: []*ipcv1.ManagedResource{
			{
				StableRole:   "base.main",
				ResourceType: ResourceMotionBase,
				AccessMode:   ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_EXCLUSIVE_CONTROL,
				RiskClass:    ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
			},
			{
				StableRole:   "arm.main",
				ResourceType: ResourceManipulatorArm,
				AccessMode:   ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_EXCLUSIVE_CONTROL,
				RiskClass:    ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
			},
		},
		Permissions: bootstrapPermissions(),
	}
	bundles := &ipcv1.InterfaceSchemaBundleSet{Bundles: []*ipcv1.InterfaceSchemaBundle{
		baseBundle,
		manipulatorBundle,
		packageBundle,
		safetyBundle,
		transferBundle,
		resourceDirBundle,
		operationBundle,
		permissionBundle,
		permissionUIBundle,
		permissionSelfBundle,
	}}
	return parseArtifacts(descriptor, bundles, "bootstrap")
}

func parseArtifacts(
	descriptor *ipcv1.ProviderDescriptor,
	bundles *ipcv1.InterfaceSchemaBundleSet,
	label string,
) (*ipcregistry.ProviderArtifacts, error) {
	marshal := proto.MarshalOptions{Deterministic: true}
	descriptorWire, err := marshal.Marshal(descriptor)
	if err != nil {
		return nil, fmt.Errorf("catalog: marshal %s descriptor: %w", label, err)
	}
	schemaWire, err := marshal.Marshal(bundles)
	if err != nil {
		return nil, fmt.Errorf("catalog: marshal %s schemas: %w", label, err)
	}
	artifacts, err := ipcregistry.ParseProviderArtifacts(descriptorWire, schemaWire)
	if err != nil {
		return nil, fmt.Errorf("catalog: validate %s artifacts: %w", label, err)
	}
	return artifacts, nil
}

func bootstrapInterface(
	interfaceID string,
	bundle *ipcv1.InterfaceSchemaBundle,
	requiredPermission string,
	riskFloor ipcv1.RiskClass,
	compatible []string,
	defaultType string,
	defaultRole string,
) *ipcv1.ProvidedInterface {
	return &ipcv1.ProvidedInterface{
		InterfaceId: interfaceID,
		InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
			Major: 1, SchemaHash: append([]byte(nil), bundle.GetSchemaHash()...),
		}},
		RequiredPermission:      requiredPermission,
		ResourceRiskFloor:       riskFloor,
		CompatibleResourceTypes: append([]string(nil), compatible...),
		DefaultResourceType:     defaultType,
		DefaultResourceRole:     defaultRole,
	}
}

func bootstrapPermissions() []*ipcv1.DefinedPermission {
	return []*ipcv1.DefinedPermission{
		bootstrapPermission(
			"perm.diagnostics.read",
			ipcv1.GrantMode_GRANT_MODE_NORMAL,
			ipcv1.RiskClass_RISK_CLASS_NORMAL,
			ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_ORDINARY,
			"",
			"",
			permText{
				NameEN: "Read diagnostics",
				DescEN: "Read this device's runtime status and diagnostic data",
			},
		),
		bootstrapPermission(
			"perm.service.register.private",
			ipcv1.GrantMode_GRANT_MODE_NORMAL,
			ipcv1.RiskClass_RISK_CLASS_NORMAL,
			ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_ORDINARY,
			"",
			"",
			permText{
				NameEN: "Register a package-private service",
				DescEN: "Register a service inside this package that only the package itself can resolve",
			},
		),
		bootstrapPermission(
			"perm.service.register",
			ipcv1.GrantMode_GRANT_MODE_PRIVILEGED,
			ipcv1.RiskClass_RISK_CLASS_NORMAL,
			ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_OEM,
			"",
			"",
			permText{
				NameEN: "Register a public system service",
				DescEN: "Register a service that other applications on this device can resolve",
			},
		),
		// perm.storage.shared is the bar for services exchanging configuration, models and caches.
		//
		// Deliberately separate from perm.storage.user: that is "the user's documents", aimed at Apps and
		// the file picker, hence USER_CONSENT + PRIVACY_SENSITIVE. Two services wanting to drop an
		// intermediate file shouldn't turn into "the user must agree to access their documents".
		//
		// GRANT_MODE_NORMAL + Ordinary: the shared area should only hold things anyone with this permission is entitled
		// to see. Data that needs a finer bar (camera frames and the like) must go through Transfer's memory
		// handles, where the handle itself is the credential and there's no filesystem path to get around it.
		bootstrapPermission(
			"perm.storage.shared",
			ipcv1.GrantMode_GRANT_MODE_NORMAL,
			ipcv1.RiskClass_RISK_CLASS_NORMAL,
			ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_ORDINARY,
			"",
			"storage",
			permText{
				NameEN: "Read and write the inter-service shared area",
				DescEN: "Exchange configuration, models and cache files between services; does not include access to your personal documents",
			},
		),
		bootstrapPermission(
			"perm.storage.user",
			ipcv1.GrantMode_GRANT_MODE_USER_CONSENT,
			ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE,
			ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_ORDINARY,
			"",
			"storage",
			permText{
				NameEN: "Access user files",
				DescEN: "Read and modify the documents, images and other files you keep on this device",
			},
		),
		bootstrapPermission(
			"perm.system.launch",
			ipcv1.GrantMode_GRANT_MODE_SYSTEM_ONLY,
			ipcv1.RiskClass_RISK_CLASS_NORMAL,
			ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_PLATFORM,
			"",
			"",
			permText{
				NameEN: "Launch system components",
				DescEN: "Start other applications and system components on this device",
			},
		),
		bootstrapPermission(
			"perm.motion.control",
			ipcv1.GrantMode_GRANT_MODE_USER_CONSENT,
			ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
			ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_ORDINARY,
			"",
			"motion",
			permText{
				NameEN: "Control robot motion",
				DescEN: "Drive the chassis to move this robot, which may cause collisions or injury",
			},
		),
		bootstrapPermission(
			"perm.manipulator.control",
			ipcv1.GrantMode_GRANT_MODE_USER_CONSENT,
			ipcv1.RiskClass_RISK_CLASS_PHYSICAL_CONTROL,
			ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_ORDINARY,
			"",
			"motion",
			permText{
				NameEN: "Control the manipulator",
				DescEN: "Move the manipulator, which may cause collisions or injury",
			},
		),
		bootstrapPermission(
			"perm.platform.control",
			ipcv1.GrantMode_GRANT_MODE_PRIVILEGED,
			ipcv1.RiskClass_RISK_CLASS_NORMAL,
			ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_PLATFORM,
			"",
			"",
			permText{
				NameEN: "Platform control",
				DescEN: "Change platform-level settings that affect the whole device",
			},
		),
		bootstrapPermission(
			"perm.authority.reboot",
			ipcv1.GrantMode_GRANT_MODE_SYSTEM_ONLY,
			ipcv1.RiskClass_RISK_CLASS_CRITICAL_SAFETY,
			ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_PLATFORM,
			rolePlatformRelease,
			"",
			permText{
				NameEN: "Emergency reboot",
				DescEN: "Reboot this device immediately without an orderly shutdown, for fault recovery",
			},
		),
		bootstrapPermission(
			"perm.authority.power",
			ipcv1.GrantMode_GRANT_MODE_PRIVILEGED,
			ipcv1.RiskClass_RISK_CLASS_CRITICAL_SAFETY,
			ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_PLATFORM,
			"",
			"",
			permText{
				NameEN: "Orderly power control",
				DescEN: "Restart or shut down this device in an orderly way",
			},
		),
		bootstrapPermission(
			"perm.safety.observe",
			ipcv1.GrantMode_GRANT_MODE_PRIVILEGED,
			ipcv1.RiskClass_RISK_CLASS_NORMAL,
			ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_OEM,
			"",
			"",
			permText{
				NameEN: "Observe safety state",
				DescEN: "Read the safety system's current state and its transitions; does not include clearing a trip",
			},
		),
		bootstrapPermission(
			"perm.safety.rearm",
			ipcv1.GrantMode_GRANT_MODE_SYSTEM_ONLY,
			ipcv1.RiskClass_RISK_CLASS_CRITICAL_SAFETY,
			ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_PLATFORM,
			rolePlatformRelease,
			"",
			permText{
				NameEN: "Re-arm the safety system",
				DescEN: "Clear the safety system's protective state after a trip, allowing the robot to move again",
			},
		),
		bootstrapPermission(
			"perm.pkg.install",
			ipcv1.GrantMode_GRANT_MODE_USER_CONSENT,
			ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE,
			ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_ORDINARY,
			"",
			"",
			permText{
				NameEN: "Install or uninstall packages",
				DescEN: "Install and uninstall applications on this device, or enable and disable their components",
			},
		),
		// perm.pkg.admin replaces what used to be hard-coded in main.go: "which Package ID may connect to the
		// admin channel". A package holding it can connect to the admin UDS and gets a writable staging directory.
		//
		// It's tightened three ways, like perm.safety.rearm, and that's no coincidence: both are abilities that
		// can change the state of the whole machine once held:
		//  SYSTEM_ONLY -> IntersectAt requires the source to be a system image package
		//  PLATFORM -> packages downgraded to Ordinary by a development build can't get it
		//  platform-release signing role -> must be signed with the platform release key
		//
		// The security boundary is exactly the same as before: what's let through is still only "who can connect to this socket".
		// Every command is still delivered to the in-process pkgregistry.Module, which re-checks signatures, digests,
		// upgrade decisions and the permission intersection. All that changed is the basis for letting it through: from a package name hard-coded in the kernel
		// to a permission the package must declare explicitly in its manifest and that has been decided on.
		bootstrapPermission(
			"perm.pkg.admin",
			ipcv1.GrantMode_GRANT_MODE_SYSTEM_ONLY,
			ipcv1.RiskClass_RISK_CLASS_CRITICAL_SAFETY,
			ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_PLATFORM,
			rolePlatformRelease,
			"",
			permText{
				NameEN: "Administer packages and grants on behalf of the system",
				DescEN: "Connect to the kernel administration channel and perform package and grant changes on behalf of the system",
			},
		),
		// perm.resource.query is the bar for device discovery.
		//
		// Same tier as perm.diagnostics.read / perm.pkg.query (NORMAL + Ordinary):
		// it exposes "what hardware is on this machine", which an App could find out anyway by probing with repeated
		// Resolve calls. The directory only turns probing into one query; it doesn't widen the exposure.
		//
		// The real bar is in using it: having the camera list doesn't mean you can get a stream; that needs perm.camera.capture.
		bootstrapPermission(
			"perm.resource.query",
			ipcv1.GrantMode_GRANT_MODE_NORMAL,
			ipcv1.RiskClass_RISK_CLASS_NORMAL,
			ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_ORDINARY,
			"",
			"",
			permText{
				NameEN: "List hardware resources on this device",
				DescEN: "List which cameras, chassis and other hardware this device has; does not include permission to use them",
			},
		),
		bootstrapPermission(
			"perm.pkg.query",
			ipcv1.GrantMode_GRANT_MODE_NORMAL,
			ipcv1.RiskClass_RISK_CLASS_NORMAL,
			ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_ORDINARY,
			"",
			"",
			permText{
				NameEN: "List installed packages",
				DescEN: "List the applications installed on this device and their components",
			},
		),
		bootstrapPermission(
			"perm.permission.admin",
			ipcv1.GrantMode_GRANT_MODE_SYSTEM_ONLY,
			ipcv1.RiskClass_RISK_CLASS_CRITICAL_SAFETY,
			ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_PLATFORM,
			rolePlatformRelease,
			"",
			permText{
				NameEN: "Manage runtime grants",
				DescEN: "View and change other applications' sensitive permission grants on behalf of the system",
			},
		),
	}
}

// permText is a permission's user-facing text.
//
// Name and Desc must be separate: on the consent screen the first is the row's title ("Access user files"), the second is
// the sentence under the title explaining what it means ("Read and modify documents you saved on this device"). They
// once shared one string, so the title and the explanation on the consent screen were identical, which is no explanation at all.
//
// Neither may be left out: it's the only thing the user reads on the consent screen.
type permText struct {
	NameEN string
	DescEN string
}

func bootstrapPermission(
	id string,
	mode ipcv1.GrantMode,
	risk ipcv1.RiskClass,
	minimum ipcv1.PermissionTrustFloor,
	requiredRole string,
	group string,
	text permText,
) *ipcv1.DefinedPermission {
	return &ipcv1.DefinedPermission{
		Id:                 id,
		GrantMode:          mode,
		RiskClass:          risk,
		MinimumTrust:       minimum,
		RequiredSignerRole: requiredRole,
		Group:              group,
		DisplayName:        &ipcv1.LocalizedText{En: text.NameEN},
		Description:        &ipcv1.LocalizedText{En: text.DescEN},
	}
}
