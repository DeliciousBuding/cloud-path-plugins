package hallpillbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/status"
)

const (
	pluginIDValue = "io.github.deliciousbuding.cloud-path-app-hall-pillbox"
	pluginVersion = "0.1.0"

	jobStartWindow   = "start-window"
	jobConfirmWindow = "confirm-window"
	jobCheckWindow   = "check-window"
	jobStatus        = "status"

	openingRequirement        = "opening"
	confirmRequirement        = "confirm"
	reminderOutputRequirement = "reminder-output"
	localDisplayRequirement   = "local-display"

	hallCap    = "cloudpath.dev/capability/hall@1"
	keyCap     = "cloudpath.dev/capability/key@1"
	buzzerCap  = "cloudpath.dev/capability/buzzer@1"
	displayCap = "cloudpath.dev/capability/display-text@1"

	buzzerAction  = "buzzer"
	displayAction = "display"

	hallCloseEvent = hallCap + "/close"
	hallAwayEvent  = hallCap + "/away"
	hallOpenEvent  = hallCap + "/open"
	keyPressEvent  = keyCap + "/press"

	windowOpened        = "opened"
	windowCompleted     = "completed"
	windowCompletedLate = "completed_late"
	windowMissed        = "missed"

	reminderPending      = "pending"
	reminderNotRequested = "not_requested"
	reminderStopped      = "stopped"

	sourceManual    = "manual"
	sourceSchedule  = "schedule"
	sourceHall      = "hall"
	sourceKey       = "key"
	sourceDashboard = "dashboard"

	reminderStartPrefix = "buzzer-start:"
	reminderStopPrefix  = "buzzer-stop:"
	displayReminderPref = "display-reminder:"
	displayIdlePref     = "display-idle:"
	displayMissedPref   = "display-missed:"
)

// windowTrack is the live state of one scheduled or manual reminder window.
type windowTrack struct {
	ID                 string
	Source             string
	ScheduleID         string
	Compartment        string
	Start              time.Time
	End                time.Time
	State              string
	OpenedAt           time.Time
	ClosedAt           time.Time
	ConfirmedAt        time.Time
	ConfirmationSource string
	MissedAt           time.Time

	HallEntity string
	KeyEntity  string

	ReminderEntity        string
	ReminderState         string
	ReminderRequestID     string
	ReminderResult        string
	ReminderErrorCode     string
	ReminderDoneAt        time.Time
	ReminderStopState     string
	ReminderStopRequestID string
	ReminderStopResult    string
	ReminderStopErrorCode string
	ReminderStopDoneAt    time.Time

	DisplayEntity    string
	DisplayTarget    string
	DisplayState     string
	DisplayRequestID string
	DisplayArgs      string
	DisplayResult    string
	DisplayErrorCode string
	DisplayDoneAt    time.Time
}

// instanceState is the per-plugin-instance runtime state.
type instanceState struct {
	config            *Config
	configRev         uint32
	bindings          map[string][]string
	windows           map[string]*windowTrack
	lastSeq           uint64
	jobs              map[string]jobOutcome
	deliveryUncertain bool
}

// Service implements Application Protocol v1 for the Hall Pillbox app.
type Service struct {
	pluginID  string
	version   string
	runtimeID string

	operationMu sync.Mutex
	mu          sync.Mutex
	initialized bool
	closed      bool
	writers     map[string]application.ApplicationEffectWriter
	effectSeq   uint64
	now         func() time.Time
	instances   map[string]*instanceState
}

var _ application.ApplicationServer = (*Service)(nil)

func ApplicationID() string { return pluginIDValue }
func Version() string       { return pluginVersion }

func New() *Service {
	return &Service{
		pluginID:  pluginIDValue,
		version:   pluginVersion,
		runtimeID: fmt.Sprintf("hall-pillbox-%d", time.Now().UnixNano()),
		now:       time.Now,
		instances: map[string]*instanceState{},
	}
}

func (s *Service) Initialize(_ context.Context, req *application.InitializeRequest) (*application.InitializeResponse, error) {
	if req == nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "nil initialize request")
	}
	negotiated := uint32(0)
	if req.ProtocolVersion == application.ProtocolVersion {
		negotiated = application.ProtocolVersion
	} else {
		for _, v := range req.SupportedProtocolVersions {
			if v == application.ProtocolVersion {
				negotiated = v
				break
			}
		}
	}
	if negotiated == 0 {
		return nil, status.Errorf(status.CodeInvalidArgument, "unsupported application protocol version %d", req.ProtocolVersion)
	}

	s.mu.Lock()
	s.initialized = true
	if strings.TrimSpace(s.runtimeID) == "" {
		s.runtimeID = fmt.Sprintf("hall-pillbox-%d", time.Now().UnixNano())
	}
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
		ApplicationID:  s.pluginID,
		Version:        s.version,
		SchemaVersions: []string{application.SchemaVersion},
		Requirements: []application.RequirementDescriptor{
			{ID: openingRequirement, Capability: hallCap, Cardinality: "one"},
			{ID: confirmRequirement, Capability: keyCap, Cardinality: "zero-or-one"},
			{ID: reminderOutputRequirement, Capability: buzzerCap, Cardinality: "one"},
			{ID: localDisplayRequirement, Capability: displayCap, Cardinality: "zero-or-one"},
		},
		Jobs:            jobDescriptors(),
		DeclarativeOnly: false,
	}, nil
}

func (s *Service) ConfigureInstance(_ context.Context, req *application.ConfigureInstanceRequest) (*application.ConfigureInstanceResponse, error) {
	if req == nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "nil configure request")
	}
	cfg, err := UnmarshalConfig(req.Config)
	if err != nil {
		return &application.ConfigureInstanceResponse{
			PluginInstanceID: req.PluginInstanceID,
			Status:           status.Errorf(status.CodeInvalidArgument, "%v", err),
		}, nil
	}

	s.mu.Lock()
	st := s.instance(req.PluginInstanceID)
	if activeCount(st) > 0 && st.config != nil && st.config.Compartment != cfg.Compartment {
		s.mu.Unlock()
		return &application.ConfigureInstanceResponse{
			PluginInstanceID: req.PluginInstanceID,
			Status:           status.Errorf(status.CodeFailedPrecondition, "cannot change compartment while a window is open"),
		}, nil
	}
	st.config = &cfg
	st.configRev = req.ConfigRevision
	s.mu.Unlock()

	return &application.ConfigureInstanceResponse{
		PluginInstanceID: req.PluginInstanceID,
		AppliedRevision:  req.ConfigRevision,
		Status:           status.New(),
	}, nil
}

func (s *Service) ValidateBinding(_ context.Context, req *application.ValidateBindingRequest) (*application.ValidateBindingResponse, error) {
	if req == nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "nil validate request")
	}
	s.mu.Lock()
	st := s.instance(req.PluginInstanceID)
	issues := validateBindings(req.Bindings, st.config)
	proposed := groupBindings(req.Bindings)
	if activeCount(st) > 0 && !sameStrings(st.bindings[openingRequirement], proposed[openingRequirement]) {
		issues = append(issues, application.BindingIssue{
			RequirementID: openingRequirement,
			Severity:      "error",
			Message:       "cannot rebind the hall opening input while a window is open",
		})
	}
	valid := len(issues) == 0
	if valid {
		st.bindings = proposed
	}
	s.mu.Unlock()
	return &application.ValidateBindingResponse{Valid: valid, Issues: issues}, nil
}

func (s *Service) HandleEvents(ctx context.Context, events application.ApplicationEventReader, effects application.ApplicationEffectWriter) error {
	if events == nil {
		return status.Errorf(status.CodeInvalidArgument, "nil event reader")
	}
	if effects == nil {
		return status.Errorf(status.CodeInvalidArgument, "nil effect writer")
	}
	defer func() {
		s.mu.Lock()
		for id, writer := range s.writers {
			if writer == effects {
				delete(s.writers, id)
			}
		}
		s.mu.Unlock()
	}()

	for {
		ev, err := events.Recv(ctx)
		if err != nil {
			if err == io.EOF || err == context.Canceled || err == context.DeadlineExceeded {
				return nil
			}
			return err
		}
		if ev != nil && ev.PluginInstanceID != "" {
			s.mu.Lock()
			if s.writers == nil {
				s.writers = map[string]application.ApplicationEffectWriter{}
			}
			s.writers[ev.PluginInstanceID] = effects
			s.mu.Unlock()
		}
		if err := s.handleEvent(ev); err != nil {
			return err
		}
	}
}

func (s *Service) handleEvent(ev *application.ApplicationEvent) error {
	if ev == nil || ev.Union == nil {
		return nil
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	s.mu.Lock()
	st := s.instance(ev.PluginInstanceID)
	if ev.Sequence != 0 && ev.Sequence <= st.lastSeq {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	var err error
	switch u := ev.Union.(type) {
	case *application.ScheduleTick:
		err = s.onScheduleTick(ev.PluginInstanceID, u)
	case *application.CapabilityEvent:
		err = s.onCapabilityEvent(ev.PluginInstanceID, u)
	case *application.RequestCompleted:
		err = s.onRequestCompleted(ev.PluginInstanceID, u)
	}
	if err == nil && ev.Sequence != 0 {
		s.mu.Lock()
		s.instance(ev.PluginInstanceID).lastSeq = ev.Sequence
		s.mu.Unlock()
	}
	return err
}

func (s *Service) onScheduleTick(instanceID string, tick *application.ScheduleTick) error {
	if tick == nil {
		return nil
	}
	payload, err := parseWindowTick(tick.WindowJSON)
	if err != nil {
		return nil
	}
	start, err := time.Parse(time.RFC3339, strings.TrimSpace(payload.Start))
	if err != nil {
		return nil
	}
	end, err := time.Parse(time.RFC3339, strings.TrimSpace(payload.End))
	if err != nil || !end.After(start) {
		return nil
	}

	s.mu.Lock()
	st := s.instance(instanceID)
	if st.config == nil {
		s.mu.Unlock()
		return nil
	}
	spec, ok := st.config.scheduleSpec(payload.ID)
	if !ok {
		s.mu.Unlock()
		return nil
	}
	w := &windowTrack{
		ID:          scheduleOccurrenceID(spec.ID, start),
		Source:      sourceSchedule,
		ScheduleID:  spec.ID,
		Compartment: st.config.resolvedCompartment(),
		Start:       start,
		End:         end,
	}
	if st.windows[w.ID] != nil || s.now().UTC().Before(start) {
		s.mu.Unlock()
		return nil
	}
	if err := s.readyForEffects(instanceID, st); err != nil {
		s.mu.Unlock()
		return err
	}
	effects := s.startWindow(st, w, s.now().UTC())
	s.mu.Unlock()
	return s.flush(instanceID, effects)
}

func (s *Service) onCapabilityEvent(instanceID string, ev *application.CapabilityEvent) error {
	if ev == nil {
		return nil
	}
	switch ev.EventType {
	case hallCloseEvent, hallAwayEvent, hallOpenEvent:
		if ev.RequirementID != openingRequirement {
			return nil
		}
		return s.confirmEvent(instanceID, ev.EntityID, ev.OccurredAt, sourceHall, true)
	case keyPressEvent:
		if ev.RequirementID != confirmRequirement {
			return nil
		}
		return s.confirmEvent(instanceID, ev.EntityID, ev.OccurredAt, sourceKey, false)
	default:
		return nil
	}
}

func (s *Service) confirmEvent(instanceID, entityID, occurredAt, source string, hall bool) error {
	s.mu.Lock()
	st := s.instance(instanceID)
	at := parseOccurred(occurredAt)
	if at.IsZero() || at.After(s.now().UTC()) {
		at = s.now().UTC()
	}
	w := newestConfirmableWindow(st, entityID, hall, at)
	if w == nil {
		s.mu.Unlock()
		return nil
	}
	if err := s.readyForEffects(instanceID, st); err != nil {
		s.mu.Unlock()
		return err
	}
	effects := s.confirmWindowEffects(st, w, at, source)
	s.mu.Unlock()
	return s.flush(instanceID, effects)
}

func (s *Service) onRequestCompleted(instanceID string, ev *application.RequestCompleted) error {
	if ev == nil {
		return nil
	}
	state := terminalCommandState(ev.State)
	if state == "" {
		return nil
	}

	s.mu.Lock()
	st := s.instance(instanceID)
	w, kind := findRequestWindow(st, ev.RequestID)
	if w == nil {
		s.mu.Unlock()
		return nil
	}
	switch kind {
	case reminderStartPrefix:
		if ev.RequestID != w.ReminderRequestID || w.ReminderState != reminderPending || ev.EntityID != w.ReminderEntity || ev.Action != buzzerAction {
			s.mu.Unlock()
			return nil
		}
		w.ReminderState = state
		w.ReminderResult = ev.ResultJSON
		w.ReminderErrorCode = ev.ErrorCode
		w.ReminderDoneAt = s.now().UTC()
	case reminderStopPrefix:
		if ev.RequestID != w.ReminderStopRequestID || w.ReminderStopState != reminderPending || ev.EntityID != w.ReminderEntity || ev.Action != buzzerAction {
			s.mu.Unlock()
			return nil
		}
		w.ReminderStopState = state
		w.ReminderStopResult = ev.ResultJSON
		w.ReminderStopErrorCode = ev.ErrorCode
		w.ReminderStopDoneAt = s.now().UTC()
	case displayReminderPref, displayIdlePref, displayMissedPref:
		if ev.RequestID != w.DisplayRequestID || w.DisplayState != reminderPending || ev.EntityID != w.DisplayEntity || ev.Action != displayAction {
			s.mu.Unlock()
			return nil
		}
		w.DisplayState = state
		w.DisplayResult = ev.ResultJSON
		w.DisplayErrorCode = ev.ErrorCode
		w.DisplayDoneAt = s.now().UTC()
	default:
		s.mu.Unlock()
		return nil
	}
	effects := []application.ApplicationEffectUnion{windowRecord(w)}
	s.mu.Unlock()
	return s.flush(instanceID, effects)
}

func (s *Service) flush(instanceID string, effects []application.ApplicationEffectUnion) error {
	for _, union := range effects {
		if err := s.sendEffect(instanceID, union); err != nil {
			s.mu.Lock()
			s.instance(instanceID).deliveryUncertain = true
			s.mu.Unlock()
			return err
		}
	}
	return nil
}

func (s *Service) sendEffect(instanceID string, union application.ApplicationEffectUnion) error {
	s.mu.Lock()
	writer := s.writers[instanceID]
	if s.closed {
		s.mu.Unlock()
		return status.Errorf(status.CodeUnavailable, "plugin is shutting down")
	}
	s.effectSeq++
	seq := s.effectSeq
	s.mu.Unlock()
	if writer == nil {
		return status.Errorf(status.CodeUnavailable, "instance has no active event/effect stream")
	}
	return writer.Send(context.Background(), &application.ApplicationEffect{
		PluginInstanceID: instanceID,
		Sequence:         seq,
		SchemaVersion:    application.SchemaVersion,
		Union:            union,
	})
}

func (s *Service) HandleRequest(_ context.Context, req *application.PluginHTTPRequest) (*application.PluginHTTPResponse, error) {
	if req == nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "nil http request")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, status.Errorf(status.CodeUnavailable, "plugin is shutting down")
	}
	if req.Method != "GET" {
		return &application.PluginHTTPResponse{
			StatusCode: 405,
			Headers:    map[string]string{"content-type": "application/json"},
			Body:       []byte(`{"error":"method_not_allowed"}`),
		}, nil
	}
	st := s.instance(req.Context.InstanceID)
	body, _ := json.Marshal(statusData(st))
	return &application.PluginHTTPResponse{
		StatusCode: 200,
		Headers:    map[string]string{"content-type": "application/json"},
		Body:       body,
	}, nil
}

func (s *Service) Health(context.Context) (*application.HealthResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := application.HealthStateServing
	if s.closed {
		state = application.HealthStateNotServing
	}
	instances := make([]application.InstanceHealth, 0, len(s.instances))
	for id, st := range s.instances {
		instanceState := state
		if st.deliveryUncertain {
			instanceState = application.HealthStateNotServing
		}
		instances = append(instances, application.InstanceHealth{PluginInstanceID: id, State: instanceState})
	}
	return &application.HealthResponse{State: state, Instances: instances}, nil
}

func (s *Service) Shutdown(_ context.Context, _ *application.ShutdownRequest) (*application.ShutdownResponse, error) {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return &application.ShutdownResponse{Status: status.New()}, nil
}

func (s *Service) instance(id string) *instanceState {
	if s.instances == nil {
		s.instances = map[string]*instanceState{}
	}
	st, ok := s.instances[id]
	if !ok {
		st = &instanceState{
			bindings: map[string][]string{},
			windows:  map[string]*windowTrack{},
			jobs:     map[string]jobOutcome{},
		}
		s.instances[id] = st
	}
	return st
}

func (s *Service) readyForEffects(instanceID string, st *instanceState) error {
	if s.closed || st.deliveryUncertain {
		return status.Errorf(status.CodeUnavailable, "instance is stopped or previous effect delivery is uncertain")
	}
	if st.config == nil || len(st.bindings[openingRequirement]) != 1 || len(st.bindings[reminderOutputRequirement]) != 1 {
		return status.Errorf(status.CodeFailedPrecondition, "configure the instance and validate opening/reminder-output bindings first")
	}
	if s.writers[instanceID] == nil {
		return status.Errorf(status.CodeUnavailable, "instance has no active event/effect stream; no action was applied")
	}
	return nil
}

func (s *Service) startWindow(st *instanceState, w *windowTrack, now time.Time) []application.ApplicationEffectUnion {
	w.Compartment = st.config.resolvedCompartment()
	w.HallEntity = firstBinding(st, openingRequirement)
	w.KeyEntity = firstBinding(st, confirmRequirement)
	w.ReminderEntity = firstBinding(st, reminderOutputRequirement)
	w.DisplayEntity = firstBinding(st, localDisplayRequirement)
	st.windows[w.ID] = w

	if !now.Before(w.End) {
		w.ReminderState = reminderNotRequested
		w.State = windowMissed
		w.MissedAt = w.End
		w.ClosedAt = w.End
		displayEffects := s.missedDisplayEffects(st, w)
		effects := []application.ApplicationEffectUnion{windowRecord(w)}
		return append(effects, displayEffects...)
	}

	w.State = windowOpened
	w.OpenedAt = now
	w.ReminderState = reminderPending
	w.ReminderRequestID = reminderStartPrefix + w.ID
	displayEffects := s.reminderDisplayEffects(st, w)
	effects := []application.ApplicationEffectUnion{
		windowRecord(w),
		&application.RequestCommand{
			EntityID:       w.ReminderEntity,
			Action:         buzzerAction,
			ArgsJSON:       mustJSON(st.config.Reminder),
			IdempotencyKey: w.ReminderRequestID,
			Deadline:       w.End.UTC().Format(time.RFC3339Nano),
		},
	}
	return append(effects, displayEffects...)
}

func (s *Service) confirmWindowEffects(st *instanceState, w *windowTrack, at time.Time, source string) []application.ApplicationEffectUnion {
	if w.State == windowCompleted || w.State == windowCompletedLate {
		return nil
	}
	if w.State == windowMissed {
		w.State = windowCompletedLate
	} else if !at.Before(w.End) {
		w.State = windowCompletedLate
		w.MissedAt = w.End
	} else {
		w.State = windowCompleted
	}
	w.ConfirmedAt = at
	w.ConfirmationSource = source
	w.ClosedAt = at

	if w.ReminderEntity != "" && w.ReminderState != reminderNotRequested {
		w.ReminderStopState = reminderPending
		w.ReminderStopRequestID = reminderStopPrefix + w.ID
	}
	displayEffects := s.idleDisplayEffects(st, w)
	effects := []application.ApplicationEffectUnion{windowRecord(w)}
	if w.ReminderStopRequestID != "" {
		effects = append(effects, &application.RequestCommand{
			EntityID:       w.ReminderEntity,
			Action:         buzzerAction,
			ArgsJSON:       mustJSON(Reminder{Freq: 0, Duration: 0}),
			IdempotencyKey: w.ReminderStopRequestID,
			Deadline:       s.now().UTC().Add(30 * time.Second).Format(time.RFC3339Nano),
		})
	}
	return append(effects, displayEffects...)
}

func (s *Service) reminderDisplayEffects(st *instanceState, w *windowTrack) []application.ApplicationEffectUnion {
	if st.config.Display == nil || w.DisplayEntity == "" {
		w.DisplayState = reminderNotRequested
		return nil
	}
	args, err := canonicalDisplayArgs(st.config.Display.ReminderArgs)
	if err != nil {
		return nil
	}
	return s.displayCommandEffects(w, displayReminderState, displayReminderPref, args, w.End)
}

func (s *Service) missedDisplayEffects(st *instanceState, w *windowTrack) []application.ApplicationEffectUnion {
	if st.config.Display == nil || w.DisplayEntity == "" || len(st.config.Display.MissedArgs) == 0 {
		w.DisplayState = reminderNotRequested
		return nil
	}
	args, err := canonicalDisplayArgs(st.config.Display.MissedArgs)
	if err != nil {
		return nil
	}
	return s.displayCommandEffects(w, displayMissedState, displayMissedPref, args, s.now().UTC().Add(30*time.Second))
}

func (s *Service) idleDisplayEffects(st *instanceState, w *windowTrack) []application.ApplicationEffectUnion {
	if st.config.Display == nil || w.DisplayEntity == "" {
		w.DisplayState = reminderNotRequested
		return nil
	}
	args, err := canonicalDisplayArgs(st.config.Display.IdleArgs)
	if err != nil {
		return nil
	}
	return s.displayCommandEffects(w, displayIdleState, displayIdlePref, args, s.now().UTC().Add(30*time.Second))
}

func (s *Service) displayCommandEffects(w *windowTrack, target, prefix, args string, deadline time.Time) []application.ApplicationEffectUnion {
	w.DisplayTarget = target
	w.DisplayState = reminderPending
	w.DisplayRequestID = prefix + w.ID
	w.DisplayArgs = args
	return []application.ApplicationEffectUnion{&application.RequestCommand{
		EntityID:       w.DisplayEntity,
		Action:         displayAction,
		ArgsJSON:       args,
		IdempotencyKey: w.DisplayRequestID,
		Deadline:       deadline.UTC().Format(time.RFC3339Nano),
	}}
}

func (s *Service) markMissed(st *instanceState, w *windowTrack) []application.ApplicationEffectUnion {
	if w.State != windowOpened {
		return nil
	}
	w.State = windowMissed
	w.MissedAt = w.End
	w.ClosedAt = w.End
	displayEffects := s.missedDisplayEffects(st, w)
	effects := []application.ApplicationEffectUnion{windowRecord(w)}
	return append(effects, displayEffects...)
}

func validateBindings(bindings []application.Binding, cfg *Config) []application.BindingIssue {
	allowed := map[string]struct{}{
		openingRequirement: {}, confirmRequirement: {}, reminderOutputRequirement: {}, localDisplayRequirement: {},
	}
	counts := map[string]int{}
	seen := map[string]bool{}
	var issues []application.BindingIssue
	for _, b := range bindings {
		if _, ok := allowed[b.RequirementID]; !ok {
			issues = append(issues, application.BindingIssue{RequirementID: b.RequirementID, Severity: "error", Message: fmt.Sprintf("requirement %q is not declared by this application", b.RequirementID)})
			continue
		}
		counts[b.RequirementID]++
		if strings.TrimSpace(b.EntityID) == "" {
			issues = append(issues, application.BindingIssue{RequirementID: b.RequirementID, Severity: "error", Message: "entity_id must not be empty"})
		}
		key := b.RequirementID + "\x00" + b.EntityID
		if seen[key] {
			issues = append(issues, application.BindingIssue{RequirementID: b.RequirementID, Severity: "error", Message: fmt.Sprintf("duplicate entity %q", b.EntityID)})
		}
		seen[key] = true
	}
	if counts[openingRequirement] != 1 {
		issues = append(issues, application.BindingIssue{RequirementID: openingRequirement, Severity: "error", Message: fmt.Sprintf("opening requires exactly one binding, got %d", counts[openingRequirement])})
	}
	if counts[reminderOutputRequirement] != 1 {
		issues = append(issues, application.BindingIssue{RequirementID: reminderOutputRequirement, Severity: "error", Message: fmt.Sprintf("reminder-output requires exactly one binding, got %d", counts[reminderOutputRequirement])})
	}
	if counts[confirmRequirement] > 1 {
		issues = append(issues, application.BindingIssue{RequirementID: confirmRequirement, Severity: "error", Message: "confirm allows at most one binding"})
	}
	if counts[localDisplayRequirement] > 1 {
		issues = append(issues, application.BindingIssue{RequirementID: localDisplayRequirement, Severity: "error", Message: "local-display allows at most one binding"})
	}
	if cfg != nil && cfg.Display != nil && counts[localDisplayRequirement] == 0 {
		issues = append(issues, application.BindingIssue{RequirementID: localDisplayRequirement, Severity: "warning", Message: "display config is present but no local-display entity is bound; display commands will not be emitted"})
	}
	return issues
}

func groupBindings(bindings []application.Binding) map[string][]string {
	out := map[string][]string{}
	for _, b := range bindings {
		out[b.RequirementID] = append(out[b.RequirementID], b.EntityID)
	}
	return out
}

func firstBinding(st *instanceState, requirement string) string {
	if st == nil {
		return ""
	}
	ids := st.bindings[requirement]
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func newestConfirmableWindow(st *instanceState, entityID string, hall bool, at time.Time) *windowTrack {
	if st == nil {
		return nil
	}
	var selected *windowTrack
	for _, w := range st.windows {
		if hall {
			if w.HallEntity != entityID {
				continue
			}
		} else if w.KeyEntity != entityID {
			continue
		}
		if at.Before(w.Start) || (!w.OpenedAt.IsZero() && at.Before(w.OpenedAt)) {
			continue
		}
		if w.State != windowOpened && w.State != windowMissed {
			continue
		}
		if selected == nil || w.Start.After(selected.Start) || (w.Start.Equal(selected.Start) && w.ID > selected.ID) {
			selected = w
		}
	}
	return selected
}

func findRequestWindow(st *instanceState, requestID string) (*windowTrack, string) {
	for _, prefix := range []string{reminderStartPrefix, reminderStopPrefix, displayReminderPref, displayIdlePref, displayMissedPref} {
		if strings.HasPrefix(requestID, prefix) {
			return st.windows[strings.TrimPrefix(requestID, prefix)], prefix
		}
	}
	return nil, ""
}

func terminalCommandState(state application.CommandState) string {
	switch state {
	case application.CommandStateSucceeded:
		return "succeeded"
	case application.CommandStateFailed:
		return "failed"
	case application.CommandStateTimedOut:
		return "timedout"
	case application.CommandStateCancelled:
		return "cancelled"
	default:
		return ""
	}
}

func activeCount(st *instanceState) int {
	if st == nil {
		return 0
	}
	count := 0
	for _, w := range st.windows {
		if w.State == windowOpened {
			count++
		}
	}
	return count
}

func windowRecord(w *windowTrack) *application.UpsertDomainRecord {
	return &application.UpsertDomainRecord{RecordType: "window", RecordID: w.ID, DataJSON: mustJSON(windowData(w)), Version: "1"}
}

func windowData(w *windowTrack) map[string]any {
	title, summary := windowPresentation(w)
	return map[string]any{
		"id":                       w.ID,
		"title":                    title,
		"summary":                  summary,
		"state":                    w.State,
		"reminder_state":           w.ReminderState,
		"opened_at":                optTime(w.OpenedAt),
		"closed_at":                optTime(w.ClosedAt),
		"confirmed_at":             optTime(w.ConfirmedAt),
		"confirmation_source":      w.ConfirmationSource,
		"missed_at":                optTime(w.MissedAt),
		"schedule_id":              w.ScheduleID,
		"compartment":              w.Compartment,
		"source":                   w.Source,
		"start":                    w.Start.UTC().Format(time.RFC3339Nano),
		"end":                      w.End.UTC().Format(time.RFC3339Nano),
		"hall_entity":              w.HallEntity,
		"key_entity":               w.KeyEntity,
		"reminder_entity":          w.ReminderEntity,
		"reminder_request_id":      w.ReminderRequestID,
		"reminder_result":          w.ReminderResult,
		"reminder_error_code":      w.ReminderErrorCode,
		"reminder_done_at":         optTime(w.ReminderDoneAt),
		"reminder_stop_state":      w.ReminderStopState,
		"reminder_stop_request_id": w.ReminderStopRequestID,
		"reminder_stop_result":     w.ReminderStopResult,
		"reminder_stop_error_code": w.ReminderStopErrorCode,
		"reminder_stop_done_at":    optTime(w.ReminderStopDoneAt),
		"display_entity":           w.DisplayEntity,
		"display_target":           w.DisplayTarget,
		"display_state":            w.DisplayState,
		"display_request_id":       w.DisplayRequestID,
		"display_args":             displayArgsJSON(w.DisplayArgs),
		"display_result":           w.DisplayResult,
		"display_error_code":       w.DisplayErrorCode,
		"display_done_at":          optTime(w.DisplayDoneAt),
	}
}

func windowPresentation(w *windowTrack) (string, string) {
	switch w.State {
	case windowOpened:
		return "药盒：待确认", "提醒窗口已开启，等待霍尔开盖或兜底确认；提醒命令结果单独记录。"
	case windowCompleted:
		return "药盒：已确认", "已在窗口内收到开盖/兜底确认；不代表药物已经吞服。"
	case windowMissed:
		return "药盒：已超时", "窗口到期仍未收到开盖/兜底确认；超时不等于已证明漏服。"
	case windowCompletedLate:
		return "药盒：迟到确认", "已在超时后收到开盖/兜底确认；原超时时间保留。"
	default:
		return "药盒：提醒窗口", "窗口状态待确认。"
	}
}

func statusData(st *instanceState) map[string]any {
	if st == nil {
		return map[string]any{"configured": false}
	}
	out := map[string]any{
		"configured":                 st.config != nil,
		"runtime_state_persistent":   false,
		"effects_delivery_uncertain": st.deliveryUncertain,
		"active":                     activeCount(st),
		"windows":                    recentWindowData(st, 100),
	}
	if st.config != nil {
		out["timezone"] = st.config.Timezone
		out["compartment"] = st.config.Compartment
		out["schedule_count"] = len(st.config.Schedule)
	}
	return out
}

func recentWindowData(st *instanceState, limit int) []map[string]any {
	if st == nil {
		return []map[string]any{}
	}
	windows := make([]*windowTrack, 0, len(st.windows))
	for _, w := range st.windows {
		windows = append(windows, w)
	}
	sort.Slice(windows, func(i, j int) bool {
		if windows[i].Start.Equal(windows[j].Start) {
			return windows[i].ID > windows[j].ID
		}
		return windows[i].Start.After(windows[j].Start)
	})
	if len(windows) > limit {
		windows = windows[:limit]
	}
	out := make([]map[string]any, 0, len(windows))
	for _, w := range windows {
		out = append(out, windowData(w))
	}
	return out
}

func windowResult(w *windowTrack) string {
	return mustJSON(map[string]any{
		"window_id":           w.ID,
		"state":               w.State,
		"reminder_state":      w.ReminderState,
		"opened_at":           optTime(w.OpenedAt),
		"closed_at":           optTime(w.ClosedAt),
		"confirmation_source": w.ConfirmationSource,
		"schedule_id":         w.ScheduleID,
		"compartment":         w.Compartment,
		"effects_status":      "submitted",
	})
}

func resultJSON(missed []string) string {
	if missed == nil {
		missed = []string{}
	}
	return mustJSON(map[string]any{"missed": missed})
}

func scheduleOccurrenceID(id string, start time.Time) string {
	return "schedule:" + id + ":" + start.UTC().Format("20060102T150405.999999999Z")
}

type windowTickPayload struct {
	ID          string `json:"id"`
	Compartment string `json:"compartment"`
	Start       string `json:"start"`
	End         string `json:"end"`
}

func parseWindowTick(raw string) (windowTickPayload, error) {
	var payload windowTickPayload
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&payload); err != nil {
		return windowTickPayload{}, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return windowTickPayload{}, fmt.Errorf("window payload must contain exactly one JSON object")
	}
	if strings.TrimSpace(payload.ID) == "" {
		return windowTickPayload{}, fmt.Errorf("window payload id is required")
	}
	return payload, nil
}

func parseOccurred(raw string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return t
}

func displayArgsJSON(raw string) any {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return json.RawMessage(raw)
}

func optTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
