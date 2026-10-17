package motionprofile

import (
	"fmt"
	"sort"

	profilev1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/motion/profile/v1"
	"google.golang.org/protobuf/proto"
)

type Instance struct {
	Profile   *profilev1.MotionProfile
	Model     string
	Resources map[string]string
}

type Issue struct {
	ProfileID      string
	Model          string
	CapabilityRole string
	Reason         string
}

type Resolution struct {
	Instances []Instance
	Issues    []Issue
}

// Resolve deterministically maps discovered models to the finite Resource
// slots in the signed bundle. Exhausted slots produce an unsupported binding
// issue; they do not make world management unavailable.
func (c *Catalog) Resolve(models []string, runtime *profilev1.RuntimeConfiguration) Resolution {
	if c == nil {
		return Resolution{}
	}
	available := append([]string(nil), models...)
	sort.Strings(available)
	if runtime != nil && len(runtime.GetInstances()) > 0 {
		return c.resolveExplicit(runtime, available)
	}

	var result Resolution
	claimed := make(map[string]struct{})
	for _, profile := range c.Profiles() {
		matched := false
		matchIndex := 0
		for _, model := range available {
			if !Matches(profile, model) {
				continue
			}
			matched = true
			resources := make(map[string]string)
			supported := true
			for _, capability := range profile.GetCapabilities() {
				roles := capability.GetResourceRoles()
				if matchIndex >= len(roles) {
					result.Issues = append(result.Issues, Issue{
						ProfileID: profile.GetProfileId(), Model: model,
						CapabilityRole: capability.GetRole(), Reason: "resource_slots_exhausted",
					})
					supported = false
					continue
				}
				role := roles[matchIndex]
				if _, duplicate := claimed[role]; duplicate {
					result.Issues = append(result.Issues, Issue{
						ProfileID: profile.GetProfileId(), Model: model,
						CapabilityRole: capability.GetRole(), Reason: "resource_slot_already_bound",
					})
					supported = false
					continue
				}
				resources[capability.GetRole()] = role
			}
			if supported {
				for _, role := range resources {
					claimed[role] = struct{}{}
				}
				result.Instances = append(result.Instances, Instance{
					Profile: proto.Clone(profile).(*profilev1.MotionProfile),
					Model:   model, Resources: resources,
				})
			}
			matchIndex++
		}
		if !matched {
			result.Issues = append(result.Issues, Issue{
				ProfileID: profile.GetProfileId(), Reason: "model_not_discovered",
			})
		}
	}
	return result
}

func (c *Catalog) resolveExplicit(
	runtime *profilev1.RuntimeConfiguration,
	models []string,
) Resolution {
	present := make(map[string]struct{}, len(models))
	for _, model := range models {
		present[model] = struct{}{}
	}
	claimed := make(map[string]struct{})
	result := Resolution{}
	for _, wire := range runtime.GetInstances() {
		profile, ok := c.Profile(wire.GetProfileId())
		if !ok {
			result.Issues = append(result.Issues, Issue{ProfileID: wire.GetProfileId(), Reason: "profile_not_found"})
			continue
		}
		if _, ok := present[wire.GetModelName()]; !ok {
			result.Issues = append(result.Issues, Issue{
				ProfileID: profile.GetProfileId(), Model: wire.GetModelName(), Reason: "model_not_discovered",
			})
			continue
		}
		assignments := make(map[string]string, len(wire.GetCapabilityResources()))
		for _, assignment := range wire.GetCapabilityResources() {
			assignments[assignment.GetCapabilityRole()] = assignment.GetResourceRole()
		}
		resources := make(map[string]string)
		supported := true
		for _, capability := range profile.GetCapabilities() {
			role := assignments[capability.GetRole()]
			if role == "" && len(capability.GetResourceRoles()) == 1 {
				role = capability.GetResourceRoles()[0]
			}
			if role == "" || !contains(capability.GetResourceRoles(), role) {
				result.Issues = append(result.Issues, Issue{
					ProfileID: profile.GetProfileId(), Model: wire.GetModelName(),
					CapabilityRole: capability.GetRole(), Reason: "resource_slot_not_selected",
				})
				supported = false
				continue
			}
			if _, duplicate := claimed[role]; duplicate {
				result.Issues = append(result.Issues, Issue{
					ProfileID: profile.GetProfileId(), Model: wire.GetModelName(),
					CapabilityRole: capability.GetRole(), Reason: "resource_slot_already_bound",
				})
				supported = false
				continue
			}
			resources[capability.GetRole()] = role
		}
		if supported {
			for _, role := range resources {
				claimed[role] = struct{}{}
			}
			result.Instances = append(result.Instances, Instance{
				Profile: profile, Model: wire.GetModelName(), Resources: resources,
			})
		}
	}
	sort.Slice(result.Instances, func(i, j int) bool {
		left := fmt.Sprintf("%s/%s", result.Instances[i].Profile.GetProfileId(), result.Instances[i].Model)
		right := fmt.Sprintf("%s/%s", result.Instances[j].Profile.GetProfileId(), result.Instances[j].Model)
		return left < right
	})
	return result
}
