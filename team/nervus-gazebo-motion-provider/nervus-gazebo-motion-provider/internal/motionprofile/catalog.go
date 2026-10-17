// Package motionprofile loads and validates signed motion Profile bundles and
// unsigned runtime instance selection. JSON is decoded with strict protojson;
// unknown fields and unsupported schema versions fail closed.
package motionprofile

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	profilev1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/motion/profile/v1"
	"github.com/nervus-os/nervus-gazebo-provider/internal/motion"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	SchemaVersion  = 1
	maxConfigBytes = 1 << 20
)

type Slot struct {
	ResourceType string
	InterfaceID  string
	Major        uint32
}

type Allowlist struct {
	Slots map[string]Slot
}

type Catalog struct {
	bundle   *profilev1.ProfileBundle
	profiles map[string]*profilev1.MotionProfile
	slots    map[string]Slot
}

func Empty() *Catalog {
	return &Catalog{
		bundle:   &profilev1.ProfileBundle{SchemaVersion: SchemaVersion},
		profiles: make(map[string]*profilev1.MotionProfile),
		slots:    make(map[string]Slot),
	}
}

func ParseBundle(data []byte, allowed Allowlist) (*Catalog, error) {
	var bundle profilev1.ProfileBundle
	if err := strictUnmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("motion profile bundle: %w", err)
	}
	if err := validateBundle(&bundle, allowed); err != nil {
		return nil, err
	}
	catalog := &Catalog{
		bundle:   proto.Clone(&bundle).(*profilev1.ProfileBundle),
		profiles: make(map[string]*profilev1.MotionProfile, len(bundle.GetProfiles())),
		slots:    make(map[string]Slot, len(bundle.GetResourceSlots())),
	}
	for _, slot := range bundle.GetResourceSlots() {
		catalog.slots[slot.GetStableRole()] = Slot{
			ResourceType: slot.GetResourceType(),
			InterfaceID:  slot.GetInterfaceId(),
			Major:        slot.GetInterfaceMajor(),
		}
	}
	for _, profile := range bundle.GetProfiles() {
		catalog.profiles[profile.GetProfileId()] =
			proto.Clone(profile).(*profilev1.MotionProfile)
	}
	return catalog, nil
}

func LoadBundle(root, relative string, allowed Allowlist) (*Catalog, error) {
	data, err := readSafeFile(root, relative)
	if err != nil {
		return nil, fmt.Errorf("motion profile bundle: %w", err)
	}
	return ParseBundle(data, allowed)
}

func (c *Catalog) Bundle() *profilev1.ProfileBundle {
	if c == nil || c.bundle == nil {
		return &profilev1.ProfileBundle{SchemaVersion: SchemaVersion}
	}
	return proto.Clone(c.bundle).(*profilev1.ProfileBundle)
}

func (c *Catalog) Profiles() []*profilev1.MotionProfile {
	if c == nil {
		return nil
	}
	ids := make([]string, 0, len(c.profiles))
	for id := range c.profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*profilev1.MotionProfile, 0, len(ids))
	for _, id := range ids {
		out = append(out, proto.Clone(c.profiles[id]).(*profilev1.MotionProfile))
	}
	return out
}

func (c *Catalog) Profile(id string) (*profilev1.MotionProfile, bool) {
	if c == nil {
		return nil, false
	}
	profile, ok := c.profiles[id]
	if !ok {
		return nil, false
	}
	return proto.Clone(profile).(*profilev1.MotionProfile), true
}

func (c *Catalog) Slot(role string) (Slot, bool) {
	if c == nil {
		return Slot{}, false
	}
	slot, ok := c.slots[role]
	return slot, ok
}

func Matches(profile *profilev1.MotionProfile, model string) bool {
	if profile == nil || profile.GetMatcher() == nil {
		return false
	}
	switch selector := profile.GetMatcher().GetSelector().(type) {
	case *profilev1.ModelMatcher_ExactName:
		return model == selector.ExactName
	case *profilev1.ModelMatcher_NamePrefix:
		return strings.HasPrefix(model, selector.NamePrefix)
	default:
		return false
	}
}

func ParseRuntime(data []byte, catalog *Catalog) (*profilev1.RuntimeConfiguration, error) {
	var config profilev1.RuntimeConfiguration
	if err := strictUnmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("motion runtime configuration: %w", err)
	}
	if err := validateRuntime(&config, catalog); err != nil {
		return nil, err
	}
	return proto.Clone(&config).(*profilev1.RuntimeConfiguration), nil
}

func LoadRuntime(root, relative string, catalog *Catalog) (*profilev1.RuntimeConfiguration, error) {
	data, err := readSafeFile(root, relative)
	if err != nil {
		return nil, fmt.Errorf("motion runtime configuration: %w", err)
	}
	return ParseRuntime(data, catalog)
}

func strictUnmarshal(data []byte, message proto.Message) error {
	if len(bytes.TrimSpace(data)) == 0 || len(data) > maxConfigBytes {
		return fmt.Errorf("configuration is empty or exceeds %d bytes", maxConfigBytes)
	}
	return (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(data, message)
}

func validateBundle(bundle *profilev1.ProfileBundle, allowed Allowlist) error {
	if bundle.GetSchemaVersion() != SchemaVersion {
		return fmt.Errorf("motion profile bundle: unsupported schema_version %d", bundle.GetSchemaVersion())
	}
	slots := make(map[string]Slot, len(bundle.GetResourceSlots()))
	for index, wire := range bundle.GetResourceSlots() {
		if wire == nil || !validRole(wire.GetStableRole()) || !validID(wire.GetResourceType()) ||
			!validID(wire.GetInterfaceId()) || wire.GetInterfaceMajor() == 0 {
			return fmt.Errorf("motion profile bundle: invalid resource slot %d", index)
		}
		if _, exists := slots[wire.GetStableRole()]; exists {
			return fmt.Errorf("motion profile bundle: duplicate resource role %q", wire.GetStableRole())
		}
		want, ok := allowed.Slots[wire.GetStableRole()]
		if !ok || want.ResourceType != wire.GetResourceType() ||
			want.InterfaceID != wire.GetInterfaceId() || want.Major != wire.GetInterfaceMajor() {
			return fmt.Errorf("motion profile bundle: resource slot %q is outside Provider descriptor", wire.GetStableRole())
		}
		slots[wire.GetStableRole()] = want
	}
	if len(slots) != len(allowed.Slots) {
		return fmt.Errorf("motion profile bundle: Resource slots do not match Provider descriptor")
	}
	for role := range allowed.Slots {
		if _, exists := slots[role]; !exists {
			return fmt.Errorf("motion profile bundle: Provider Resource %q is missing", role)
		}
	}
	profiles := make(map[string]struct{}, len(bundle.GetProfiles()))
	for index, profile := range bundle.GetProfiles() {
		if err := validateProfile(profile, slots); err != nil {
			return fmt.Errorf("motion profile bundle: profile %d: %w", index, err)
		}
		if _, exists := profiles[profile.GetProfileId()]; exists {
			return fmt.Errorf("motion profile bundle: duplicate profile_id %q", profile.GetProfileId())
		}
		profiles[profile.GetProfileId()] = struct{}{}
	}
	return nil
}

func validateProfile(profile *profilev1.MotionProfile, slots map[string]Slot) error {
	if profile == nil || profile.GetSchemaVersion() != SchemaVersion ||
		!validID(profile.GetProfileId()) || len(profile.GetDescription()) > 1024 {
		return fmt.Errorf("invalid profile identity or schema version")
	}
	if !validMatcher(profile.GetMatcher()) {
		return fmt.Errorf("invalid model matcher")
	}
	frames := make(map[string]struct{}, len(profile.GetFrames()))
	for _, frame := range profile.GetFrames() {
		if frame == nil || !validRole(frame.GetRole()) || !validGazeboToken(frame.GetName()) {
			return fmt.Errorf("invalid frame")
		}
		if _, exists := frames[frame.GetRole()]; exists {
			return fmt.Errorf("duplicate frame role %q", frame.GetRole())
		}
		frames[frame.GetRole()] = struct{}{}
	}
	groups := make(map[string]*profilev1.JointGroup, len(profile.GetJointGroups()))
	for _, group := range profile.GetJointGroups() {
		if err := validateJointGroup(group); err != nil {
			return err
		}
		if _, exists := groups[group.GetRole()]; exists {
			return fmt.Errorf("duplicate joint group role %q", group.GetRole())
		}
		groups[group.GetRole()] = group
	}
	commands := make(map[string]*profilev1.CommandBinding, len(profile.GetCommandBindings()))
	for _, binding := range profile.GetCommandBindings() {
		if err := validateCommand(binding); err != nil {
			return err
		}
		if _, exists := commands[binding.GetRole()]; exists {
			return fmt.Errorf("duplicate command binding role %q", binding.GetRole())
		}
		commands[binding.GetRole()] = binding
	}
	sources := make(map[string]*profilev1.StateSource, len(profile.GetStateSources()))
	for _, source := range profile.GetStateSources() {
		if source == nil || !validRole(source.GetRole()) {
			return fmt.Errorf("invalid state source")
		}
		if _, exists := sources[source.GetRole()]; exists {
			return fmt.Errorf("duplicate state source role %q", source.GetRole())
		}
		sources[source.GetRole()] = source
	}
	homes := make(map[string]*profilev1.HomeState, len(profile.GetHomeStates()))
	for _, home := range profile.GetHomeStates() {
		group := groups[home.GetJointGroup()]
		if home == nil || !validRole(home.GetRole()) || group == nil ||
			len(home.GetPositionRad()) != len(group.GetJoints()) || !finiteSlice(home.GetPositionRad()) {
			return fmt.Errorf("invalid home state")
		}
		if _, exists := homes[home.GetRole()]; exists {
			return fmt.Errorf("duplicate home state role %q", home.GetRole())
		}
		homes[home.GetRole()] = home
	}
	capabilities := make(map[string]struct{}, len(profile.GetCapabilities()))
	claimedResources := make(map[string]string)
	for _, capability := range profile.GetCapabilities() {
		if capability == nil || !validRole(capability.GetRole()) {
			return fmt.Errorf("invalid capability role")
		}
		if _, exists := capabilities[capability.GetRole()]; exists {
			return fmt.Errorf("duplicate capability role %q", capability.GetRole())
		}
		capabilities[capability.GetRole()] = struct{}{}
		spec, ok := motion.Lookup(capability.GetAdapter())
		if !ok || !spec.Semantics[capability.GetSemantic()] {
			return fmt.Errorf("unsupported adapter/semantic for capability %q", capability.GetRole())
		}
		expectedInterface := motion.ExpectedInterface(capability.GetSemantic())
		if capability.GetInterfaceId() != expectedInterface || capability.GetInterfaceMajor() != 1 {
			return fmt.Errorf("capability %q has an invalid interface", capability.GetRole())
		}
		if len(capability.GetResourceRoles()) == 0 {
			return fmt.Errorf("capability %q has no signed Resource slot", capability.GetRole())
		}
		seenResource := make(map[string]struct{}, len(capability.GetResourceRoles()))
		for _, role := range capability.GetResourceRoles() {
			slot, exists := slots[role]
			if !exists || slot.InterfaceID != expectedInterface || slot.Major != 1 {
				return fmt.Errorf("capability %q references incompatible Resource %q", capability.GetRole(), role)
			}
			if _, duplicate := seenResource[role]; duplicate {
				return fmt.Errorf("capability %q repeats Resource %q", capability.GetRole(), role)
			}
			seenResource[role] = struct{}{}
			if owner, duplicate := claimedResources[role]; duplicate {
				return fmt.Errorf("Resource %q is shared by capabilities %q and %q", role, owner, capability.GetRole())
			}
			claimedResources[role] = capability.GetRole()
		}
		command := commands[capability.GetCommandBinding()]
		if command == nil || command.GetAdapter() != capability.GetAdapter() {
			return fmt.Errorf("capability %q has no compatible command binding", capability.GetRole())
		}
		if (capability.GetAdapter() == profilev1.AdapterKind_ADAPTER_KIND_JOINT_POSITION ||
			capability.GetAdapter() == profilev1.AdapterKind_ADAPTER_KIND_JOINT_TRAJECTORY) &&
			groups[capability.GetJointGroup()] == nil {
			return fmt.Errorf("capability %q has no joint group", capability.GetRole())
		}
		if capability.GetBaseFrame() != "" {
			if _, ok := frames[capability.GetBaseFrame()]; !ok {
				return fmt.Errorf("capability %q references an unknown base frame", capability.GetRole())
			}
		}
		if capability.GetEndFrame() != "" {
			if _, ok := frames[capability.GetEndFrame()]; !ok {
				return fmt.Errorf("capability %q references an unknown end frame", capability.GetRole())
			}
		}
		if capability.GetHomeState() != "" && homes[capability.GetHomeState()] == nil {
			return fmt.Errorf("capability %q references an unknown home state", capability.GetRole())
		}
		for _, role := range capability.GetStateSources() {
			source := sources[role]
			wantType, supported := spec.StateTypes[source.GetKind()]
			if source == nil || !supported || source.GetMessageType() != wantType ||
				motion.ValidateTemplate(source.GetTopicTemplate(), spec.StateTokens) != nil {
				return fmt.Errorf("capability %q has an invalid state source %q", capability.GetRole(), role)
			}
		}
		if !validMotionLimits(capability.GetLimits()) {
			return fmt.Errorf("capability %q has invalid limits", capability.GetRole())
		}
	}
	return nil
}

func validateCommand(binding *profilev1.CommandBinding) error {
	if binding == nil || !validRole(binding.GetRole()) {
		return fmt.Errorf("invalid command binding")
	}
	spec, ok := motion.Lookup(binding.GetAdapter())
	if !ok || binding.GetMessageType() != spec.CommandType {
		return fmt.Errorf("command binding %q overrides adapter message type", binding.GetRole())
	}
	switch spec.Direction {
	case motion.DirectionPublish:
		if binding.GetServiceTemplate() != "" || motion.ValidateTemplate(binding.GetTopicTemplate(), spec.CommandTokens) != nil {
			return fmt.Errorf("command binding %q has an invalid publish template", binding.GetRole())
		}
	case motion.DirectionRequest:
		if binding.GetTopicTemplate() != "" || motion.ValidateTemplate(binding.GetServiceTemplate(), spec.CommandTokens) != nil {
			return fmt.Errorf("command binding %q has an invalid service template", binding.GetRole())
		}
	default:
		return fmt.Errorf("command binding %q has no direction", binding.GetRole())
	}
	return nil
}

func validateJointGroup(group *profilev1.JointGroup) error {
	if group == nil || !validRole(group.GetRole()) || len(group.GetJoints()) == 0 || len(group.GetJoints()) > 4096 {
		return fmt.Errorf("invalid joint group")
	}
	roles := make(map[string]struct{}, len(group.GetJoints()))
	names := make(map[string]struct{}, len(group.GetJoints()))
	for _, joint := range group.GetJoints() {
		if joint == nil || !validRole(joint.GetRole()) || !validGazeboToken(joint.GetName()) ||
			joint.GetAxis() > 15 || !finiteJoint(joint) {
			return fmt.Errorf("invalid joint in group %q", group.GetRole())
		}
		if joint.GetHasPositionLimits() && joint.GetLowerPositionRad() >= joint.GetUpperPositionRad() {
			return fmt.Errorf("joint %q has invalid position limits", joint.GetName())
		}
		key := fmt.Sprintf("%s/%d", joint.GetName(), joint.GetAxis())
		if _, exists := roles[joint.GetRole()]; exists {
			return fmt.Errorf("duplicate joint role %q", joint.GetRole())
		}
		if _, exists := names[key]; exists {
			return fmt.Errorf("duplicate joint axis %q", key)
		}
		roles[joint.GetRole()] = struct{}{}
		names[key] = struct{}{}
	}
	return nil
}

func validateRuntime(config *profilev1.RuntimeConfiguration, catalog *Catalog) error {
	if config.GetSchemaVersion() != SchemaVersion {
		return fmt.Errorf("motion runtime configuration: unsupported schema_version %d", config.GetSchemaVersion())
	}
	if config.GetWorld() != "" && !validGazeboToken(config.GetWorld()) {
		return fmt.Errorf("motion runtime configuration: invalid world")
	}
	if config.GetPartition() != "" && !validGazeboToken(config.GetPartition()) {
		return fmt.Errorf("motion runtime configuration: invalid partition")
	}
	if config.GetDiscoveryTimeoutMs() > 120_000 || config.GetRequestTimeoutMs() > 120_000 {
		return fmt.Errorf("motion runtime configuration: timeout exceeds 120000ms")
	}
	switch config.GetLogLevel() {
	case "", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("motion runtime configuration: invalid log_level")
	}
	instances := make(map[string]struct{}, len(config.GetInstances()))
	claimedResources := make(map[string]string)
	for _, instance := range config.GetInstances() {
		profile, ok := catalog.Profile(instance.GetProfileId())
		if !ok || !validGazeboToken(instance.GetModelName()) || !Matches(profile, instance.GetModelName()) {
			return fmt.Errorf("motion runtime configuration: invalid profile/model binding")
		}
		key := instance.GetProfileId() + "\x00" + instance.GetModelName()
		if _, duplicate := instances[key]; duplicate {
			return fmt.Errorf("motion runtime configuration: duplicate profile instance")
		}
		instances[key] = struct{}{}
		capabilities := capabilityMap(profile)
		assignments := make(map[string]string, len(instance.GetCapabilityResources()))
		for _, assignment := range instance.GetCapabilityResources() {
			capability := capabilities[assignment.GetCapabilityRole()]
			if capability == nil || !contains(capability.GetResourceRoles(), assignment.GetResourceRole()) {
				return fmt.Errorf("motion runtime configuration: invalid capability Resource assignment")
			}
			if _, duplicate := assignments[assignment.GetCapabilityRole()]; duplicate {
				return fmt.Errorf("motion runtime configuration: duplicate capability assignment")
			}
			assignments[assignment.GetCapabilityRole()] = assignment.GetResourceRole()
		}
		for role, capability := range capabilities {
			resource := assignments[role]
			if resource == "" && len(capability.GetResourceRoles()) == 1 {
				resource = capability.GetResourceRoles()[0]
			}
			if resource == "" {
				return fmt.Errorf("motion runtime configuration: capability %q needs an explicit Resource", role)
			}
			if owner, duplicate := claimedResources[resource]; duplicate {
				return fmt.Errorf("motion runtime configuration: Resource %q is already assigned to %s", resource, owner)
			}
			claimedResources[resource] = key + "/" + role
		}
	}
	return nil
}

func capabilityMap(profile *profilev1.MotionProfile) map[string]*profilev1.MotionCapability {
	out := make(map[string]*profilev1.MotionCapability, len(profile.GetCapabilities()))
	for _, capability := range profile.GetCapabilities() {
		out[capability.GetRole()] = capability
	}
	return out
}

func readSafeFile(root, relative string) ([]byte, error) {
	if root == "" || relative == "" || filepath.IsAbs(relative) {
		return nil, fmt.Errorf("configuration path must be relative to an explicit root")
	}
	clean := filepath.Clean(relative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("configuration path escapes root")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return nil, err
	}
	fileReal, err := filepath.EvalSymlinks(filepath.Join(rootReal, clean))
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(rootReal, fileReal)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("configuration symlink escapes root")
	}
	file, err := os.Open(fileReal)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
}

var (
	idPattern   = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	rolePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	namePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]{0,254}$`)
)

func validID(value string) bool   { return idPattern.MatchString(value) }
func validRole(value string) bool { return rolePattern.MatchString(value) }
func validGazeboToken(value string) bool {
	return namePattern.MatchString(value) && !strings.Contains(value, "..")
}

func validMatcher(matcher *profilev1.ModelMatcher) bool {
	if matcher == nil {
		return false
	}
	switch selector := matcher.GetSelector().(type) {
	case *profilev1.ModelMatcher_ExactName:
		return validGazeboToken(selector.ExactName)
	case *profilev1.ModelMatcher_NamePrefix:
		return validGazeboToken(selector.NamePrefix)
	default:
		return false
	}
}

func finiteJoint(joint *profilev1.JointSpec) bool {
	return finite(joint.GetLowerPositionRad()) && finite(joint.GetUpperPositionRad()) &&
		finite(joint.GetMaxVelocityRadS()) && finite(joint.GetPositionToleranceRad()) &&
		finite(joint.GetHomePositionRad()) && joint.GetMaxVelocityRadS() >= 0 &&
		joint.GetPositionToleranceRad() >= 0
}

func validMotionLimits(limits *profilev1.MotionLimits) bool {
	if limits == nil {
		return true
	}
	values := []float64{limits.GetMaxLinearMS(), limits.GetMaxAngularRadS(),
		limits.GetMaxJointVelocityRadS(), limits.GetPositionToleranceRad()}
	for _, value := range values {
		if !finite(value) || value < 0 {
			return false
		}
	}
	return limits.GetSettleTimeMs() <= 60_000
}

func finiteSlice(values []float64) bool {
	for _, value := range values {
		if !finite(value) {
			return false
		}
	}
	return true
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
