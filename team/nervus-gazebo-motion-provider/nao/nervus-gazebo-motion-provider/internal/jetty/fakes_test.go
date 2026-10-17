package jetty

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nervus-os/nervus-gazebo-provider/internal/gzmsgs"
	"github.com/nervus-os/nervus-gazebo-provider/internal/gztransport"
	"github.com/nervus-os/nervus-gazebo-provider/internal/motionprofile"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"google.golang.org/protobuf/proto"
)

type fakeClient struct {
	mu sync.Mutex

	world                       string
	model                       string
	feedback                    bool
	accepted                    bool
	commandTopicInfoUnavailable bool
	paused                      bool
	iter                        uint64

	topics        map[string]gztransport.TopicInfo
	subscriptions map[gztransport.SubscriptionID]string
	nextID        gztransport.SubscriptionID
	sequence      uint64
	events        chan gztransport.Event
	eventErrors   chan error
	closed        chan struct{}
	closeOnce     sync.Once
	online        bool

	sceneWire []byte
	requests  []*gzmsgs.WorldControl
	published []gztransport.Message
}

func newFakeClient(tinyFeedback bool) *fakeClient {
	world := "nervus_home"
	model := "nervus_nao"
	scene, _ := proto.Marshal(&gzmsgs.Scene{
		Name: world,
		Model: []*gzmsgs.Model{{
			Name: model,
			Id:   42,
			Link: []*gzmsgs.Link{{Name: "Torso", Id: 43, Canonical: true}},
			Joint: []*gzmsgs.Joint{{
				Name: "HeadYaw",
				Id:   44,
				Type: gzmsgs.Joint_REVOLUTE,
				Axis1: &gzmsgs.Axis{
					LimitLower: -2.08567,
					LimitUpper: 2.08567,
				},
			}},
		}},
	})
	f := &fakeClient{
		world:         world,
		model:         model,
		feedback:      tinyFeedback,
		accepted:      true,
		paused:        false,
		topics:        make(map[string]gztransport.TopicInfo),
		subscriptions: make(map[gztransport.SubscriptionID]string),
		events:        make(chan gztransport.Event, 128),
		eventErrors:   make(chan error, 8),
		closed:        make(chan struct{}),
		online:        true,
		sceneWire:     scene,
	}
	f.topics[statsTopic(world)] = gztransport.TopicInfo{
		PublisherCount: 1,
		MessageType:    typeWorldStatistics,
	}
	f.topics[jointCommandTopic(model, "HeadYaw", 0)] = gztransport.TopicInfo{
		SubscriberCount: 1,
		MessageType:     typeDouble,
	}
	if tinyFeedback {
		f.topics[jointStateTopic(world, model)] = gztransport.TopicInfo{
			PublisherCount: 1,
			MessageType:    typeModel,
		}
	}
	return f
}

func (f *fakeClient) Versions() gztransport.Versions {
	return gztransport.Versions{ABI: 1, Transport: "15.test", Messages: "12.test"}
}

func (f *fakeClient) ListTopics(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.online {
		return nil, gztransport.ErrClosed
	}
	out := make([]string, 0, len(f.topics))
	for topic := range f.topics {
		out = append(out, topic)
	}
	return out, nil
}

func (f *fakeClient) ListServices(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.online {
		return nil, gztransport.ErrClosed
	}
	return []string{controlService(f.world), sceneService(f.world)}, nil
}

func (f *fakeClient) TopicInfo(_ context.Context, topic string) (gztransport.TopicInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.online {
		return gztransport.TopicInfo{}, gztransport.ErrClosed
	}
	if f.commandTopicInfoUnavailable && topic == jointCommandTopic(f.model, "HeadYaw", 0) {
		return gztransport.TopicInfo{}, gztransport.ErrNotFound
	}
	info, ok := f.topics[topic]
	if !ok {
		return gztransport.TopicInfo{}, &gztransport.Error{
			Status:    gztransport.StatusNotFound,
			Operation: "fake topic info",
		}
	}
	return info, nil
}

func (f *fakeClient) ServiceInfo(_ context.Context, service string) (gztransport.ServiceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.online {
		return gztransport.ServiceInfo{}, gztransport.ErrClosed
	}
	switch service {
	case controlService(f.world):
		return gztransport.ServiceInfo{
			ProviderCount: 1,
			RequestType:   typeWorldControl,
			ResponseType:  typeBoolean,
		}, nil
	case sceneService(f.world):
		return gztransport.ServiceInfo{
			ProviderCount: 1,
			RequestType:   typeEmpty,
			ResponseType:  typeScene,
		}, nil
	default:
		return gztransport.ServiceInfo{}, gztransport.ErrNotFound
	}
}

func (f *fakeClient) WaitForTopic(ctx context.Context, topic, messageType string) error {
	info, err := f.TopicInfo(ctx, topic)
	if err != nil {
		return err
	}
	if info.MessageType != messageType {
		return gztransport.ErrTypeMismatch
	}
	return nil
}

func (f *fakeClient) WaitForService(
	ctx context.Context,
	service, requestType, responseType string,
) error {
	info, err := f.ServiceInfo(ctx, service)
	if err != nil {
		return err
	}
	if info.RequestType != requestType || info.ResponseType != responseType {
		return gztransport.ErrTypeMismatch
	}
	return nil
}

func (f *fakeClient) Publish(
	ctx context.Context,
	message gztransport.Message,
	_ gztransport.PublishOptions,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var value gzmsgs.Double
	if message.Type != typeDouble || proto.Unmarshal(message.Data, &value) != nil {
		return gztransport.ErrTypeMismatch
	}
	f.mu.Lock()
	if !f.online {
		f.mu.Unlock()
		return gztransport.ErrClosed
	}
	f.published = append(f.published, gztransport.Message{
		Topic: message.Topic,
		Type:  message.Type,
		Data:  append([]byte(nil), message.Data...),
	})
	feedback := f.feedback
	f.mu.Unlock()
	if feedback {
		f.enqueueJointState(value.GetData())
	}
	return nil
}

func (f *fakeClient) Request(
	ctx context.Context,
	request gztransport.ServiceRequest,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	online := f.online
	f.mu.Unlock()
	if !online {
		return nil, gztransport.ErrClosed
	}
	if request.Service == sceneService(f.world) {
		return append([]byte(nil), f.sceneWire...), nil
	}
	if request.Service != controlService(f.world) {
		return nil, gztransport.ErrNotFound
	}
	var control gzmsgs.WorldControl
	if request.RequestType != typeWorldControl || request.ResponseType != typeBoolean ||
		proto.Unmarshal(request.Data, &control) != nil {
		return nil, gztransport.ErrTypeMismatch
	}
	f.mu.Lock()
	f.requests = append(f.requests, proto.Clone(&control).(*gzmsgs.WorldControl))
	accepted := f.accepted
	if accepted {
		switch {
		case control.GetMultiStep() > 0:
			f.iter += uint64(control.GetMultiStep())
		case control.GetReset_() != nil:
			f.iter = 0
		case control.GetPause():
			f.paused = true
		default:
			f.paused = false
		}
	}
	paused, iterations := f.paused, f.iter
	f.mu.Unlock()
	if accepted {
		f.enqueueStats(paused, iterations)
	}
	return proto.Marshal(&gzmsgs.Boolean{Data: accepted})
}

func (f *fakeClient) Subscribe(
	ctx context.Context,
	topic, messageType string,
	_ gztransport.SubscriptionOptions,
) (gztransport.SubscriptionID, error) {
	if err := f.WaitForTopic(ctx, topic, messageType); err != nil {
		return 0, err
	}
	f.mu.Lock()
	f.nextID++
	id := f.nextID
	f.subscriptions[id] = topic
	paused, iterations := f.paused, f.iter
	f.mu.Unlock()
	if topic == statsTopic(f.world) {
		f.enqueueStatsFor(id, paused, iterations)
	}
	return id, nil
}

func (f *fakeClient) Unsubscribe(_ context.Context, id gztransport.SubscriptionID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.subscriptions[id]; !ok {
		return gztransport.ErrNotFound
	}
	delete(f.subscriptions, id)
	return nil
}

func (f *fakeClient) NextEvent(ctx context.Context) (gztransport.Event, error) {
	select {
	case event := <-f.events:
		return event, nil
	case err := <-f.eventErrors:
		return gztransport.Event{}, err
	case <-f.closed:
		return gztransport.Event{}, gztransport.ErrClosed
	case <-ctx.Done():
		return gztransport.Event{}, ctx.Err()
	}
}

func (f *fakeClient) disconnect() {
	f.mu.Lock()
	f.online = false
	f.subscriptions = make(map[gztransport.SubscriptionID]string)
	f.mu.Unlock()
	f.eventErrors <- gztransport.ErrClosed
}

func (f *fakeClient) reconnect() {
	f.mu.Lock()
	f.online = true
	f.mu.Unlock()
}

func (f *fakeClient) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeClient) enqueueStats(paused bool, iterations uint64) {
	f.mu.Lock()
	var id gztransport.SubscriptionID
	for candidate, topic := range f.subscriptions {
		if topic == statsTopic(f.world) {
			id = candidate
			break
		}
	}
	f.mu.Unlock()
	if id != 0 {
		f.enqueueStatsFor(id, paused, iterations)
	}
}

func (f *fakeClient) enqueueStatsFor(
	id gztransport.SubscriptionID,
	paused bool,
	iterations uint64,
) {
	nanos := int64(iterations) * 4_000_000
	payload, _ := proto.Marshal(&gzmsgs.WorldStatistics{
		SimTime:        &gzmsgs.Time{Sec: nanos / 1_000_000_000, Nsec: int32(nanos % 1_000_000_000)},
		RealTime:       &gzmsgs.Time{Sec: nanos / 1_000_000_000, Nsec: int32(nanos % 1_000_000_000)},
		Paused:         paused,
		Iterations:     iterations,
		RealTimeFactor: 1,
	})
	f.enqueue(id, statsTopic(f.world), typeWorldStatistics, payload)
}

func (f *fakeClient) enqueueJointState(position float64) {
	f.mu.Lock()
	var id gztransport.SubscriptionID
	for candidate, topic := range f.subscriptions {
		if topic == jointStateTopic(f.world, f.model) {
			id = candidate
			break
		}
	}
	f.mu.Unlock()
	if id == 0 {
		return
	}
	payload, _ := proto.Marshal(&gzmsgs.Model{
		Name: f.model,
		Joint: []*gzmsgs.Joint{{
			Name:  "HeadYaw",
			Axis1: &gzmsgs.Axis{Position: position, Velocity: 0.01},
		}},
	})
	f.enqueue(id, jointStateTopic(f.world, f.model), typeModel, payload)
}

func (f *fakeClient) enqueue(
	id gztransport.SubscriptionID,
	topic, messageType string,
	payload []byte,
) {
	f.mu.Lock()
	f.sequence++
	sequence := f.sequence
	f.mu.Unlock()
	f.events <- gztransport.Event{
		SubscriptionID: id,
		Sequence:       sequence,
		Topic:          topic,
		MessageType:    messageType,
		Data:           payload,
	}
}

func (f *fakeClient) lastPublishedDouble() (string, float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.published) == 0 {
		return "", 0, errors.New("no published messages")
	}
	last := f.published[len(f.published)-1]
	var value gzmsgs.Double
	if err := proto.Unmarshal(last.Data, &value); err != nil {
		return "", 0, err
	}
	return last.Topic, value.GetData(), nil
}

var _ gztransport.Client = (*fakeClient)(nil)

type fakeProcessController struct {
	mu       sync.Mutex
	snapshot simulation.ProcessSnapshot
	worlds   []string
	events   chan simulation.ProcessEvent
	starts   int
	stops    int
	closed   bool
}

func newFakeManagedProcess(worlds ...string) *fakeProcessController {
	return &fakeProcessController{
		snapshot: simulation.ProcessSnapshot{State: simulation.ProcessStopped},
		worlds:   append([]string(nil), worlds...),
		events:   make(chan simulation.ProcessEvent, 4),
	}
}

func (f *fakeProcessController) Mode() simulation.Mode { return simulation.ModeManaged }

func (f *fakeProcessController) AllowedWorlds() []string {
	return append([]string(nil), f.worlds...)
}

func (f *fakeProcessController) Snapshot() simulation.ProcessSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot
}

func (f *fakeProcessController) Start(
	_ context.Context,
	world string,
) (simulation.ProcessSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapshot.State == simulation.ProcessRunning {
		return simulation.ProcessSnapshot{}, simulation.NewError(
			simulation.ReasonProcessAlreadyRunning, "fake.start", false,
		)
	}
	f.starts++
	f.snapshot = simulation.ProcessSnapshot{
		State: simulation.ProcessRunning, Owned: true, ProcessID: 4242, World: world,
	}
	return f.snapshot, nil
}

func (f *fakeProcessController) Stop(context.Context) (simulation.ProcessSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapshot.State != simulation.ProcessRunning {
		return simulation.ProcessSnapshot{}, simulation.NewError(
			simulation.ReasonProcessNotRunning, "fake.stop", false,
		)
	}
	f.stops++
	f.snapshot.State = simulation.ProcessStopped
	f.snapshot.Owned = false
	f.snapshot.ProcessID = 0
	f.snapshot.HasLastExitCode = true
	f.snapshot.LastExitCode = 0
	return f.snapshot, nil
}

func (f *fakeProcessController) Events() <-chan simulation.ProcessEvent { return f.events }

func (f *fakeProcessController) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.events)
	}
	return nil
}

type fakeWSLgProbe struct{ faults []simulation.WSLgFaultEvent }

func (f fakeWSLgProbe) Check(context.Context) []simulation.WSLgFaultEvent {
	return append([]simulation.WSLgFaultEvent(nil), f.faults...)
}

var _ simulation.ProcessController = (*fakeProcessController)(nil)

func testConfig(feedback bool) (Config, *fakeClient) {
	config := DefaultConfig()
	config.Profiles = mustTestProfiles()
	config.DiscoveryTimeout = 2 * time.Second
	config.DiscoveryRetryInterval = 10 * time.Millisecond
	config.StatsStaleTimeout = 10 * time.Second
	return config, newFakeClient(feedback)
}

func mustTestProfiles() *motionprofile.Catalog {
	const bundle = `{
	  "schemaVersion": 1,
	  "resourceSlots": [{
	    "stableRole": "simulation.world.main",
	    "resourceType": "gazebo.simulation.motion.provider.resource.world",
	    "interfaceId": "gazebo.simulation.motion.provider.control",
	    "interfaceMajor": 1
	  }],
	  "profiles": [{
	    "schemaVersion": 1,
	    "profileId": "gazebo.test.nao.v1",
	    "matcher": {"exactName": "nervus_nao"},
	    "jointGroups": [{"role": "all", "joints": [{
	      "role": "head_yaw", "name": "HeadYaw", "axis": 0,
	      "positionToleranceRad": 0.01
	    }]}],
	    "commandBindings": [{
	      "role": "position", "adapter": "ADAPTER_KIND_JOINT_POSITION",
	      "topicTemplate": "/model/{model}/joint/{joint}/{axis}/cmd_pos",
	      "messageType": "gz.msgs.Double"
	    }],
	    "stateSources": [{
	      "role": "joint_state", "kind": "STATE_SOURCE_KIND_JOINT_STATE",
	      "topicTemplate": "/world/{world}/model/{model}/joint_state",
	      "messageType": "gz.msgs.Model", "optional": true
	    }],
	    "capabilities": [{
	      "role": "compatibility.joints",
	      "semantic": "CAPABILITY_SEMANTIC_COMPATIBILITY_JOINT_CONTROL",
	      "adapter": "ADAPTER_KIND_JOINT_POSITION",
	      "interfaceId": "gazebo.simulation.motion.provider.control",
	      "interfaceMajor": 1,
	      "resourceRoles": ["simulation.world.main"],
	      "jointGroup": "all", "commandBinding": "position",
	      "stateSources": ["joint_state"]
	    }]
	  }]
	}`
	catalog, err := motionprofile.ParseBundle([]byte(bundle), motionprofile.Allowlist{
		Slots: map[string]motionprofile.Slot{
			"simulation.world.main": {
				ResourceType: "gazebo.simulation.motion.provider.resource.world",
				InterfaceID:  "gazebo.simulation.motion.provider.control",
				Major:        1,
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return catalog
}
