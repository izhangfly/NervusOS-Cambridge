// Package main is camerad's providergen: it produces this service's data-driven Provider contract
// (provider.binpb + schemas.binpb), for sysmanifest to include in the digests and write into the manifest.
//
// # One fundamental difference from pkgmanagerd's providergen
//
// That one is [purely static]: fixed interfaces and no resources. This one reads the board JSON:
// how many cameras a board has, what they're called and which way they face are board facts, not code constants.
//
// So the same board.json is read twice:
//
//	build time  this program reads it → generates the signed nervus.resource.camera declarations
//	run time    camerad reads it      → maps roles to real device nodes
//
// [It must be the same file]. With two, "the Catalog has cam.front" and "which device cam.front
// points at at run time" would become two facts that can drift independently, and after drifting neither side
// reports an error; an App asking for the front camera just gets an endpoint that doesn't exist.
//
// # Why the resource set must be fixed at build time
//
// ProviderArtifacts go into the manifest's digests and are signed with the image. If resources were read fresh
// from JSON at run time, editing one line of JSON would add yourself a "front camera"; something the signature doesn't cover
// isn't a contract.
//
// The cost is repackaging for a new board. That's right: the board config is part of the image anyway.
//
// # Why what this package declares needn't be compared with the kernel bootstrap
//
// pkgmanagerd's must replicate the same-named interface in the kernel bootstrap word for word (the kernel also defines
// nervus.interface.pkg.manager). The kernel [knows nothing about cameras]: not one line of
// kernel code mentions camera. This package is the only source of these definitions, which is exactly the shape the data-driven Catalog
// is meant to have.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	camerav1 "github.com/nervus-os/nervus-ipc/protocol/interface/camerav1"
	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	ipcregistry "github.com/nervus-os/nervus-ipc/registry"

	"github.com/nervus-os/nervus-system-server/camerad/boardprofile"
)

const (
	packageID = "nervus.camerad"

	interfaceCamera       = "nervus.interface.camera"
	interfaceCameraConfig = "nervus.interface.camera.config"
	interfaceMajor        = 1

	resourceCamera       = "nervus.resource.camera"
	resourceCameraConfig = "nervus.resource.camera.config"

	permissionCapture   = "perm.camera.capture"
	permissionConfigure = "perm.camera.configure"

	// File names must match the provider section of manifest.json.in
	descriptorFileName = "provider.binpb"
	schemasFileName    = "schemas.binpb"
)

func main() {
	out := flag.String("out", "", "output directory (provider.binpb and schemas.binpb are written there)")
	profilePath := flag.String("profile", "", "board camera configuration JSON")
	flag.Parse()

	if err := run(*out, *profilePath); err != nil {
		fmt.Fprintf(os.Stderr, "providergen(%s): %v\n", packageID, err)
		os.Exit(1)
	}
}

func run(out, profilePath string) error {
	if out == "" {
		return fmt.Errorf("-out is required")
	}
	if profilePath == "" {
		return fmt.Errorf("-profile is required")
	}

	profile, err := boardprofile.Load(profilePath)
	if err != nil {
		return err
	}

	descriptorWire, schemaWire, err := BuildArtifacts(profile)
	if err != nil {
		return err
	}

	// 0o644: these two files are covered by digests and ship read-only in the image; no executable bit needed
	if err := os.WriteFile(filepath.Join(out, descriptorFileName), descriptorWire, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", descriptorFileName, err)
	}
	if err := os.WriteFile(filepath.Join(out, schemasFileName), schemaWire, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", schemasFileName, err)
	}

	fmt.Printf("✅ %s provider contract (board %s)\n", packageID, profile.Board)
	fmt.Printf("   %-16s %d bytes\n", descriptorFileName, len(descriptorWire))
	fmt.Printf("   %-16s %d bytes\n", schemasFileName, len(schemaWire))
	fmt.Printf("   resources        %d capture + %d config\n",
		len(profile.AllRoles()), len(profile.AllConfigRoles()))
	return nil
}

// BuildArtifacts is exported separately so tests can assert the contract's shape without writing to disk.
func BuildArtifacts(profile *boardprofile.Profile) (descriptorWire, schemaWire []byte, err error) {
	// The method and event enums both come from the generated code: method_id, event_id and their metadata
	// all hang off the enum values, and the cost of copying them locally isn't duplication, it's that the copy quietly goes stale.
	//
	// [Events go through the bundle, not inline in the descriptor]: the inline route is for metadata interfaces
	// (without a schema), which doesn't allow payload_type, and the camera's events all carry payloads.
	captureBundle, err := ipcregistry.BuildSchemaBundleWithEvents(
		interfaceCamera, interfaceMajor,
		camerav1.CameraMethod(0).Descriptor(),
		camerav1.CameraEvent(0).Descriptor())
	if err != nil {
		return nil, nil, fmt.Errorf("build %s schema bundle: %w", interfaceCamera, err)
	}
	configBundle, err := ipcregistry.BuildSchemaBundleWithEvents(
		interfaceCameraConfig, interfaceMajor,
		camerav1.CameraConfigMethod(0).Descriptor(),
		camerav1.CameraConfigEvent(0).Descriptor())
	if err != nil {
		return nil, nil, fmt.Errorf("build %s schema bundle: %w", interfaceCameraConfig, err)
	}

	descriptor := &ipcv1.ProviderDescriptor{
		PackageId: packageID,
		Interfaces: []*ipcv1.ProvidedInterface{
			{
				InterfaceId: interfaceCamera,
				InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
					Major:      interfaceMajor,
					SchemaHash: append([]byte(nil), captureBundle.GetSchemaHash()...),
				}},
				RequiredPermission: permissionCapture,
				// Risk floor: resources bound to this interface may not be below PRIVACY_SENSITIVE.
				// It stops "a vendor declaring a NORMAL camera resource", which would let streaming
				// bypass the privacy-sensitive tier of decisions.
				ResourceRiskFloor:       ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE,
				CompatibleResourceTypes: []string{resourceCamera},
				// [No default_resource_type/role]: a machine with several cameras is
				// normal, and an implicit default would make "I want a camera" silently resolve to one of them. Which one
				// has to be said explicitly.
			},
			{
				InterfaceId: interfaceCameraConfig,
				InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
					Major:      interfaceMajor,
					SchemaHash: append([]byte(nil), configBundle.GetSchemaHash()...),
				}},
				RequiredPermission:      permissionConfigure,
				ResourceRiskFloor:       ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE,
				CompatibleResourceTypes: []string{resourceCameraConfig},
			},
		},
		Resources:   resourcesOf(profile),
		Permissions: permissions(),
	}

	set := &ipcv1.InterfaceSchemaBundleSet{
		Bundles: []*ipcv1.InterfaceSchemaBundle{captureBundle, configBundle},
	}
	return ipcregistry.MarshalProviderArtifacts(descriptor, set)
}

// resourcesOf turns the board config into resource declarations.
//
// Each role produces [two] resources: capture (shared observation) and config (exclusive control).
// Their stable_roles differ (config adds a .config suffix), because the Catalog's handle
// index is globally unique across types, and the same name would collide on a handle at setup.
//
// [Including pool slots with no device plugged in right now]: resource declarations answer "what could this board have";
// "what it has right now" is expressed by RegisterEndpoint. The other way round, plugging in a second camera would need
// a repackage.
func resourcesOf(profile *boardprofile.Profile) []*ipcv1.ManagedResource {
	roles := profile.AllRoles()
	out := make([]*ipcv1.ManagedResource, 0, len(roles)*2)

	for _, role := range roles {
		out = append(out, &ipcv1.ManagedResource{
			StableRole:   role,
			ResourceType: resourceCamera,
			// [Shared observation]: navigation, recording and AI detection watching one stream at once is normal,
			// and "exclusive control" can't express that.
			AccessMode: ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_SHARED_OBSERVE,
			RiskClass:  ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE,
			// Semantic labels come from the board config. Pool slots have no labels: nobody knows which way a camera plugged in
			// on the fly faces, and giving it one would let Apps pick a device facing who knows where.
			Labels: profile.LabelsFor(role),
		})
		out = append(out, &ipcv1.ManagedResource{
			StableRole:   boardprofile.ConfigRole(role),
			ResourceType: resourceCameraConfig,
			// [Exclusive control]: with two processes changing exposure at once, who wins depends on timing,
			// and the picture flips between the two values.
			AccessMode: ipcv1.ResourceAccessMode_RESOURCE_ACCESS_MODE_EXCLUSIVE_CONTROL,
			RiskClass:  ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE,
			// [The config side has no semantic labels]: selecting by label is the capture side's job. Which camera the config side
			// configures is worked out by the caller from the capture role (adding .config).
			// Labels on both would make facing=front match two resources of different types,
			// and REQUIRE_UNIQUE would then fail.
		})
	}
	return out
}

// permissions declares the two platform permissions this service introduces.
//
// [This package may define perm.*]: it's signed platform-release. A vendor package signed oem-service
// defining perm.* would be refused by authorizePermissionNamespace; that's how "standard semantics come from the platform"
// is enforced.
func permissions() []*ipcv1.DefinedPermission {
	return []*ipcv1.DefinedPermission{
		{
			Id: permissionCapture,
			// USER_CONSENT: this is [the camera starting to film you]. Nothing needs the user's nod more,
			// and installing something shouldn't agree to it on their behalf.
			GrantMode:    ipcv1.GrantMode_GRANT_MODE_USER_CONSENT,
			RiskClass:    ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE,
			MinimumTrust: ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_ORDINARY,
			Group:        "camera",
			DisplayName:  &ipcv1.LocalizedText{ZhCn: "Use the camera", En: "Use the camera"},
			Description: &ipcv1.LocalizedText{
				ZhCn: "Take pictures and record video", En: "Take pictures and record video",
			},
		},
		{
			Id: permissionConfigure,
			// USER_CONSENT too, not a looser tier: whoever can change exposure can turn the picture
			// completely black, which is safety-relevant for a robot that navigates by sight.
			GrantMode:    ipcv1.GrantMode_GRANT_MODE_USER_CONSENT,
			RiskClass:    ipcv1.RiskClass_RISK_CLASS_PRIVACY_SENSITIVE,
			MinimumTrust: ipcv1.PermissionTrustFloor_PERMISSION_TRUST_FLOOR_ORDINARY,
			Group:        "camera",
			DisplayName:  &ipcv1.LocalizedText{ZhCn: "Adjust camera settings", En: "Adjust camera settings"},
			Description: &ipcv1.LocalizedText{
				ZhCn: "Adjust exposure, gain, white balance", En: "Adjust exposure, gain, white balance",
			},
		},
	}
}
