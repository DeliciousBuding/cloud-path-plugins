package musicplayer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/status"
)

// Manifest identity. These values must mirror plugin.yaml.
const (
	pluginIDValue = "io.github.deliciousbuding.cloud-path-app-music-player"
	pluginVersion = "0.2.7"

	soundRequirement     = "sound"
	displayRequirement   = "local-display"
	indicatorRequirement = "indicator"

	soundCapability   = "cloudpath.dev/capability/buzzer@1"
	displayCapability = "cloudpath.dev/capability/display-text@1"
	indicatorCap      = "cloudpath.dev/capability/led@1"

	toneAction         = "tone"
	toneSequenceAction = "tone-sequence"
)

const (
	statusIdle      = "idle"
	statusQueued    = "queued"
	statusPlaying   = "playing"
	statusCompleted = "completed"
	statusFailed    = "failed"

	commandQueued    = "queued"
	commandSucceeded = "succeeded"
	commandFailed    = "failed"
	commandTimedOut  = "timed_out"
	commandCancelled = "cancelled"

	sessionRecordType = "music_session"
	sessionRecordID   = "current"
	sessionRecordVer  = "1"

	maxIdempotencyKeyBytes = 256
	maxJobResults          = 128
)

type sessionCommand struct {
	Key            string
	Action         string
	ArgsJSON       string
	Notes          []Note
	StartNoteIndex int
	State          string
}

type musicSession struct {
	RequestID      string
	JobID          string
	Song           string
	Status         string
	QueuedAt       time.Time
	LastNote       *NoteResult
	Repeat         int
	TotalNotes     int
	CompletedNotes int
	Commands       map[string]*sessionCommand
	CommandOrder   []string
	NextIndex      int
	ErrorCode      string
	ResultJSON     string
	FailedNote     *NoteResult
}

type jobOutcome struct {
	JobID      string
	ArgsJSON   string
	ResultJSON string
}

type instanceState struct {
	id                string
	config            Config
	configRev         uint32
	configured        bool
	bindings          map[string][]string
	bindingsValid     bool
	session           *musicSession
	jobs              map[string]jobOutcome
	jobOrder          []string
	deliveryUncertain bool
	lastSeq           uint64
}

func newInstanceState(id string) *instanceState {
	return &instanceState{
		id:       id,
		bindings: map[string][]string{},
		jobs:     map[string]jobOutcome{},
	}
}

// Service implements Application Protocol v1. It is deliberately device
// agnostic: it only sends capability actions to entity IDs supplied by Core.
type Service struct {
	pluginID  string
	version   string
	runtimeID string

	// operationMu serializes business transitions and their effect batches.
	operationMu sync.Mutex
	mu          sync.Mutex
	initialized bool
	closed      bool
	now         func() time.Time
	instances   map[string]*instanceState

	writers       map[string]application.ApplicationEffectWriter
	defaultWriter application.ApplicationEffectWriter
	streamCount   int
	effectSeq     uint64
	idCounter     atomic.Uint64
}

var _ application.ApplicationServer = (*Service)(nil)

func ApplicationID() string { return pluginIDValue }
func Version() string       { return pluginVersion }

func New() *Service {
	return &Service{
		pluginID:  pluginIDValue,
		version:   pluginVersion,
		runtimeID: fmt.Sprintf("music-player-%d", time.Now().UnixNano()),
		now:       time.Now,
		instances: map[string]*instanceState{},
		writers:   map[string]application.ApplicationEffectWriter{},
	}
}

func (s *Service) Initialize(_ context.Context, req *application.InitializeRequest) (*application.InitializeResponse, error) {
	if req == nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "nil initialize request")
	}
	if req.PluginID != "" && req.PluginID != pluginIDValue {
		return nil, status.Errorf(status.CodeInvalidArgument, "plugin id mismatch")
	}
	if req.PluginVersion != "" && req.PluginVersion != pluginVersion {
		return nil, status.Errorf(status.CodeInvalidArgument, "plugin version mismatch")
	}
	negotiated := uint32(0)
	if req.ProtocolVersion == application.ProtocolVersion {
		negotiated = application.ProtocolVersion
	} else {
		for _, version := range req.SupportedProtocolVersions {
			if version == application.ProtocolVersion {
				negotiated = version
				break
			}
		}
	}
	if negotiated == 0 {
		return nil, status.Errorf(status.CodeInvalidArgument, "unsupported application protocol version %d", req.ProtocolVersion)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, status.Errorf(status.CodeUnavailable, "plugin is shutting down")
	}
	s.initialized = true
	runtimeID := s.runtimeID
	s.mu.Unlock()

	return &application.InitializeResponse{
		NegotiatedProtocolVersion: negotiated,
		Status:                    status.New(),
		RuntimeID:                 runtimeID,
	}, nil
}

func (s *Service) Describe(context.Context) (*application.ApplicationDescriptor, error) {
	return &application.ApplicationDescriptor{
		ApplicationID:  pluginIDValue,
		Version:        pluginVersion,
		SchemaVersions: []string{application.SchemaVersion},
		Requirements: []application.RequirementDescriptor{
			{ID: soundRequirement, Capability: soundCapability, Cardinality: "one"},
			{ID: displayRequirement, Capability: displayCapability, Cardinality: "zero-or-one"},
			{ID: indicatorRequirement, Capability: indicatorCap, Cardinality: "zero-or-one"},
		},
		Jobs:            jobDescriptors(),
		DeclarativeOnly: false,
	}, nil
}

func (s *Service) ConfigureInstance(_ context.Context, req *application.ConfigureInstanceRequest) (*application.ConfigureInstanceResponse, error) {
	if req == nil || strings.TrimSpace(req.PluginInstanceID) == "" {
		return nil, status.Errorf(status.CodeInvalidArgument, "configure requires an instance id")
	}
	cfg, err := UnmarshalConfig(req.Config)
	if err != nil {
		return &application.ConfigureInstanceResponse{
			PluginInstanceID: req.PluginInstanceID,
			Status:           status.Errorf(status.CodeInvalidArgument, "invalid config: %v", err),
		}, nil
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, status.Errorf(status.CodeUnavailable, "plugin is shutting down")
	}
	st := s.instanceLocked(req.PluginInstanceID)
	st.config = cfg
	st.configRev = req.ConfigRevision
	st.configured = true
	s.mu.Unlock()

	return &application.ConfigureInstanceResponse{
		PluginInstanceID: req.PluginInstanceID,
		AppliedRevision:  req.ConfigRevision,
		Status:           status.New(),
	}, nil
}

func (s *Service) ValidateBinding(_ context.Context, req *application.ValidateBindingRequest) (*application.ValidateBindingResponse, error) {
	if req == nil || strings.TrimSpace(req.PluginInstanceID) == "" {
		return nil, status.Errorf(status.CodeInvalidArgument, "binding validation requires an instance id")
	}
	issues := validateBindings(req.Bindings)
	valid := len(issues) == 0

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, status.Errorf(status.CodeUnavailable, "plugin is shutting down")
	}
	st := s.instanceLocked(req.PluginInstanceID)
	if valid {
		proposed := groupBindings(req.Bindings)
		if hasActiveSession(st) && !sameBindings(st.bindings, proposed) {
			valid = false
			issues = append(issues, application.BindingIssue{
				RequirementID: soundRequirement,
				Severity:      "error",
				Message:       "cannot rebind while a music session is queued or playing",
			})
		} else {
			st.bindings = proposed
			st.bindingsValid = true
		}
	}
	s.mu.Unlock()

	return &application.ValidateBindingResponse{Valid: valid, Issues: issues}, nil
}

func (s *Service) HandleEvents(ctx context.Context, events application.ApplicationEventReader, effects application.ApplicationEffectWriter) error {
	if events == nil || effects == nil {
		return status.Errorf(status.CodeInvalidArgument, "event stream requires a reader and writer")
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return status.Errorf(status.CodeUnavailable, "plugin is shutting down")
	}
	s.streamCount++
	if s.streamCount == 1 {
		s.defaultWriter = effects
	}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if s.defaultWriter == effects {
			s.defaultWriter = nil
		}
		for id, writer := range s.writers {
			if writer == effects {
				delete(s.writers, id)
			}
		}
		if s.streamCount > 0 {
			s.streamCount--
		}
		s.mu.Unlock()
	}()

	for {
		event, err := events.Recv(ctx)
		if err != nil {
			if err == io.EOF || err == context.Canceled || err == context.DeadlineExceeded {
				return nil
			}
			return err
		}
		if event == nil {
			continue
		}
		if event.PluginInstanceID != "" {
			s.mu.Lock()
			s.writers[event.PluginInstanceID] = effects
			s.mu.Unlock()
		}
		if err := s.handleEvent(event); err != nil {
			return err
		}
	}
}

func (s *Service) handleEvent(event *application.ApplicationEvent) error {
	if event == nil || event.Union == nil {
		return nil
	}

	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return status.Errorf(status.CodeUnavailable, "plugin is shutting down")
	}
	st := s.instanceLocked(event.PluginInstanceID)
	if event.Sequence != 0 && event.Sequence <= st.lastSeq {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	var err error
	switch value := event.Union.(type) {
	case *application.RequestCompleted:
		err = s.onRequestCompleted(event.PluginInstanceID, value)
	}
	if err == nil && event.Sequence != 0 {
		s.mu.Lock()
		st.lastSeq = event.Sequence
		s.mu.Unlock()
	}
	return err
}

func (s *Service) sendEffects(instanceID string, effects []application.ApplicationEffectUnion) error {
	if len(effects) == 0 {
		return nil
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return status.Errorf(status.CodeUnavailable, "plugin is shutting down")
	}
	writer := s.writerForLocked(instanceID)
	if writer == nil {
		s.mu.Unlock()
		return status.Errorf(status.CodeUnavailable, "instance has no active event/effect stream")
	}
	baseSequence := s.effectSeq
	s.effectSeq += uint64(len(effects))
	s.mu.Unlock()

	for index, union := range effects {
		effect := &application.ApplicationEffect{
			PluginInstanceID: instanceID,
			Sequence:         baseSequence + uint64(index) + 1,
			SchemaVersion:    application.SchemaVersion,
			Union:            union,
		}
		if err := writer.Send(context.Background(), effect); err != nil {
			s.mu.Lock()
			if st := s.instances[instanceID]; st != nil {
				st.deliveryUncertain = true
			}
			s.mu.Unlock()
			return err
		}
	}
	return nil
}

func (s *Service) HandleRequest(_ context.Context, req *application.PluginHTTPRequest) (*application.PluginHTTPResponse, error) {
	if req == nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "nil HTTP request")
	}
	instanceID := req.Context.InstanceID
	if instanceID == "" {
		instanceID = req.PluginInstanceID
	}
	if instanceID == "" {
		return nil, status.Errorf(status.CodeInvalidArgument, "host instance context is missing")
	}
	if req.Method != "GET" {
		return &application.PluginHTTPResponse{
			StatusCode: 405,
			Headers:    map[string]string{"content-type": "application/json; charset=utf-8", "allow": "GET"},
			Body:       []byte(`{"error":"method_not_allowed"}`),
		}, nil
	}
	if req.Path != "" && req.Path != "/" && req.Path != "/status" {
		return &application.PluginHTTPResponse{
			StatusCode: 404,
			Headers:    map[string]string{"content-type": "application/json; charset=utf-8"},
			Body:       []byte(`{"error":"not_found"}`),
		}, nil
	}

	s.mu.Lock()
	st := s.instanceLocked(instanceID)
	body := mustJSON(sessionData(st))
	s.mu.Unlock()
	return &application.PluginHTTPResponse{
		StatusCode: 200,
		Headers:    map[string]string{"content-type": "application/json; charset=utf-8", "cache-control": "no-store"},
		Body:       []byte(body),
	}, nil
}

func (s *Service) Health(context.Context) (*application.HealthResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state := application.HealthStateServing
	if !s.initialized || s.closed {
		state = application.HealthStateNotServing
	}
	ids := make([]string, 0, len(s.instances))
	for id := range s.instances {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	response := &application.HealthResponse{State: state}
	for _, id := range ids {
		st := s.instances[id]
		instanceState := state
		detail := "status=" + statusIdle
		if st.session != nil {
			detail = "status=" + st.session.Status
		}
		if !st.configured || !st.bindingsValid || st.deliveryUncertain {
			instanceState = application.HealthStateNotServing
		}
		response.Instances = append(response.Instances, application.InstanceHealth{
			PluginInstanceID: id,
			State:            instanceState,
			Detail:           detail,
		})
	}
	return response, nil
}

func (s *Service) Shutdown(context.Context, *application.ShutdownRequest) (*application.ShutdownResponse, error) {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return &application.ShutdownResponse{Status: status.New()}, nil
}

func (s *Service) instanceLocked(id string) *instanceState {
	if s.instances == nil {
		s.instances = map[string]*instanceState{}
	}
	st := s.instances[id]
	if st == nil {
		st = newInstanceState(id)
		s.instances[id] = st
	}
	return st
}

func (s *Service) writerForLocked(instanceID string) application.ApplicationEffectWriter {
	if writer := s.writers[instanceID]; writer != nil {
		return writer
	}
	if s.streamCount == 1 {
		return s.defaultWriter
	}
	return nil
}

func validateBindings(bindings []application.Binding) []application.BindingIssue {
	allowed := map[string]struct{}{
		soundRequirement:     {},
		displayRequirement:   {},
		indicatorRequirement: {},
	}
	counts := map[string]int{}
	seen := map[string]bool{}
	var issues []application.BindingIssue

	for _, binding := range bindings {
		if _, ok := allowed[binding.RequirementID]; !ok {
			issues = append(issues, application.BindingIssue{
				RequirementID: binding.RequirementID,
				Severity:      "error",
				Message:       fmt.Sprintf("requirement %q is not declared by this application", binding.RequirementID),
			})
			continue
		}
		if strings.TrimSpace(binding.EntityID) == "" {
			issues = append(issues, application.BindingIssue{
				RequirementID: binding.RequirementID,
				Severity:      "error",
				Message:       "entity_id must not be empty",
			})
			continue
		}
		key := binding.RequirementID + "\x00" + binding.EntityID
		if seen[key] {
			issues = append(issues, application.BindingIssue{
				RequirementID: binding.RequirementID,
				Severity:      "error",
				Message:       fmt.Sprintf("duplicate binding for entity %q", binding.EntityID),
			})
		}
		seen[key] = true
		counts[binding.RequirementID]++
	}

	if counts[soundRequirement] != 1 {
		issues = append(issues, application.BindingIssue{
			RequirementID: soundRequirement,
			Severity:      "error",
			Message:       fmt.Sprintf("sound requires exactly one binding, got %d", counts[soundRequirement]),
		})
	}
	for _, optional := range []string{displayRequirement, indicatorRequirement} {
		if counts[optional] > 1 {
			issues = append(issues, application.BindingIssue{
				RequirementID: optional,
				Severity:      "error",
				Message:       fmt.Sprintf("%s allows at most one binding, got %d", optional, counts[optional]),
			})
		}
	}
	return issues
}

func hasActiveSession(st *instanceState) bool {
	if st == nil || st.session == nil {
		return false
	}
	return st.session.Status == statusQueued || st.session.Status == statusPlaying
}

func sameBindings(current, proposed map[string][]string) bool {
	if len(current) != len(proposed) {
		return false
	}
	for requirement, currentEntities := range current {
		proposedEntities := proposed[requirement]
		if len(currentEntities) != len(proposedEntities) {
			return false
		}
		matched := make([]bool, len(proposedEntities))
		for _, entity := range currentEntities {
			found := false
			for index, candidate := range proposedEntities {
				if !matched[index] && candidate == entity {
					matched[index] = true
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
	}
	return true
}

func groupBindings(bindings []application.Binding) map[string][]string {
	out := map[string][]string{}
	for _, binding := range bindings {
		out[binding.RequirementID] = append(out[binding.RequirementID], binding.EntityID)
	}
	return out
}

func soundEntity(st *instanceState) string {
	if st == nil {
		return ""
	}
	entities := st.bindings[soundRequirement]
	if len(entities) != 1 {
		return ""
	}
	return entities[0]
}

func terminalCommandState(state application.CommandState) string {
	switch state {
	case application.CommandStateSucceeded:
		return commandSucceeded
	case application.CommandStateFailed:
		return commandFailed
	case application.CommandStateTimedOut:
		return commandTimedOut
	case application.CommandStateCancelled:
		return commandCancelled
	default:
		return ""
	}
}

func mustJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}
