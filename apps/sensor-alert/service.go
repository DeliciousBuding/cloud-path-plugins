// Package sensoralert implements a device-independent Application plugin that
// evaluates sensor observations and capability events, then requests generic
// tone/LED effects. It never opens hardware, serial ports or network sockets.
package sensoralert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/status"
)

const (
	pluginID      = "io.github.deliciousbuding.cloud-path-app-sensor-alert"
	pluginVersion = "0.1.2"

	jobArm            = "arm"
	jobDisarm         = "disarm"
	jobStatus         = "status"
	jobCheckFreshness = "check-freshness"

	emptyObjectSchema = `{"type":"object","properties":{},"additionalProperties":false}`
	maxJobResults     = 128
)

func ApplicationID() string { return pluginID }
func Version() string       { return pluginVersion }

type effectRoute struct {
	writer application.ApplicationEffectWriter
	owner  *int
}

// Service shares a process, never business state, across plugin instances.
type Service struct {
	mu        sync.Mutex
	instances map[string]*instanceState
	routes    map[string]*effectRoute
	now       func() time.Time

	initialized atomic.Bool
	closed      atomic.Bool
	runtimeID   string
}

var _ application.ApplicationServer = (*Service)(nil)

func New() *Service {
	return &Service{
		instances: map[string]*instanceState{},
		routes:    map[string]*effectRoute{},
		now:       time.Now,
	}
}

func (s *Service) Initialize(_ context.Context, req *application.InitializeRequest) (*application.InitializeResponse, error) {
	if req == nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "nil initialize request")
	}
	if s.closed.Load() {
		return nil, status.Errorf(status.CodeUnavailable, "plugin is shutting down")
	}
	if req.PluginID != "" && req.PluginID != pluginID {
		return nil, status.Errorf(status.CodeInvalidArgument, "plugin id mismatch")
	}
	if req.PluginVersion != "" && req.PluginVersion != pluginVersion {
		return nil, status.Errorf(status.CodeInvalidArgument, "plugin version mismatch")
	}
	compatible := req.ProtocolVersion == application.ProtocolVersion
	for _, version := range req.SupportedProtocolVersions {
		compatible = compatible || version == application.ProtocolVersion
	}
	if !compatible {
		return nil, status.Errorf(status.CodeInvalidArgument, "unsupported application protocol version")
	}
	s.mu.Lock()
	if s.runtimeID == "" {
		s.runtimeID = fmt.Sprintf("sensor-alert-%d", s.now().UnixNano())
	}
	runtimeID := s.runtimeID
	s.mu.Unlock()
	s.initialized.Store(true)
	return &application.InitializeResponse{NegotiatedProtocolVersion: application.ProtocolVersion, Status: status.New(), RuntimeID: runtimeID}, nil
}

func (s *Service) Describe(context.Context) (*application.ApplicationDescriptor, error) {
	requirements := make([]application.RequirementDescriptor, 0, len(roles))
	for _, role := range roles {
		requirements = append(requirements, application.RequirementDescriptor{
			ID: role, Capability: capabilityForRole(role), Cardinality: "zero-or-one",
		})
	}
	return &application.ApplicationDescriptor{
		ApplicationID: pluginID, Version: pluginVersion, SchemaVersions: []string{application.SchemaVersion},
		Requirements: requirements,
		Jobs: []application.JobDescriptor{
			{ID: jobArm, Title: "布防告警", InputSchemaJSON: emptyObjectSchema, ManualOnly: true},
			{ID: jobDisarm, Title: "撤防告警", InputSchemaJSON: emptyObjectSchema, ManualOnly: true},
			{ID: jobStatus, Title: "读取告警状态", InputSchemaJSON: emptyObjectSchema, ManualOnly: true},
			{ID: jobCheckFreshness, Title: "检查绑定传感器新鲜度", InputSchemaJSON: emptyObjectSchema},
		},
	}, nil
}

func validInstanceID(id string) bool {
	return id != "" && len(id) <= 256 && strings.TrimSpace(id) == id
}

func (s *Service) lookup(id string, create bool) (*instanceState, error) {
	if !validInstanceID(id) {
		return nil, status.Errorf(status.CodeInvalidArgument, "instance id is required")
	}
	if s.closed.Load() {
		return nil, status.Errorf(status.CodeUnavailable, "plugin is shutting down")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.instances[id]
	if st == nil && create {
		st = newInstance(id)
		s.instances[id] = st
	}
	if st == nil {
		return nil, status.Errorf(status.CodeNotFound, "instance is not configured")
	}
	return st, nil
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
	st, err := s.lookup(req.PluginInstanceID, true)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.configured && (req.ConfigRevision < st.configRev || (req.ConfigRevision == st.configRev && !cfg.sameAs(st.config))) {
		return &application.ConfigureInstanceResponse{
			PluginInstanceID: req.PluginInstanceID,
			AppliedRevision:  st.configRev,
			Status:           status.Errorf(status.CodeFailedPrecondition, "config revision is stale or reused with different content"),
		}, nil
	}
	if !st.configured || req.ConfigRevision != st.configRev {
		previousActive := st.active
		previousLastTriggered := st.lastTriggered
		st.config = cfg
		st.configRev = req.ConfigRevision
		st.configured = true
		st.sensors = map[string]*sensorState{}
		st.active = map[string]activeAlert{}
		st.lastTriggered = map[string]time.Time{}
		// A configuration update must not erase an unresolved alert. Keep
		// active conditions that remain enabled, then let the next observation
		// either recover them or leave them active.
		if st.armed {
			for key, active := range previousActive {
				if !cfg.sensorEnabled(active.Sensor) {
					continue
				}
				st.active[key] = active
				if triggeredAt, ok := previousLastTriggered[key]; ok {
					st.lastTriggered[key] = triggeredAt
				}
			}
		}
		st.pending = map[string]commandResult{}
		st.lastCommand = nil
		st.jobResults = map[string]string{}
		st.jobOrder = nil
		if st.armed {
			if len(st.active) == 0 {
				st.state = stateArmed
				st.alert = AlertRecord{State: stateArmed, Severity: "info", Summary: "sensor alert armed"}
			}
		} else {
			st.state = stateDisarmed
			st.alert = AlertRecord{State: stateDisarmed, Severity: "info", Summary: "sensor alert disarmed"}
		}
	}
	return &application.ConfigureInstanceResponse{PluginInstanceID: req.PluginInstanceID, AppliedRevision: st.configRev, Status: status.New()}, nil
}

func validateBindings(bindings []application.Binding) (map[string]string, []application.BindingIssue) {
	out := map[string]string{}
	var issues []application.BindingIssue
	issue := func(role, message string) {
		issues = append(issues, application.BindingIssue{RequirementID: role, Severity: "error", Message: message})
	}
	for _, b := range bindings {
		if capabilityForRole(b.RequirementID) == "" {
			issue(b.RequirementID, "undeclared requirement")
			continue
		}
		if !validInstanceID(b.EntityID) {
			issue(b.RequirementID, "entity_id must be a non-empty trimmed id")
			continue
		}
		if _, exists := out[b.RequirementID]; exists {
			issue(b.RequirementID, "zero-or-one requirement has duplicate binding")
			continue
		}
		out[b.RequirementID] = b.EntityID
	}
	return out, issues
}

func sameBindings(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func (s *Service) ValidateBinding(_ context.Context, req *application.ValidateBindingRequest) (*application.ValidateBindingResponse, error) {
	if req == nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "nil validate request")
	}
	if !validInstanceID(req.PluginInstanceID) {
		return nil, status.Errorf(status.CodeInvalidArgument, "instance id is required")
	}
	bindings, issues := validateBindings(req.Bindings)
	if len(issues) != 0 {
		return &application.ValidateBindingResponse{Valid: false, Issues: issues}, nil
	}
	st, err := s.lookup(req.PluginInstanceID, true)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	changed := !st.bindingsValid || !sameBindings(st.bindings, bindings)
	st.bindings = bindings
	st.bindingsValid = true
	if changed {
		st.sensors = map[string]*sensorState{}
		st.active = map[string]activeAlert{}
		st.lastTriggered = map[string]time.Time{}
		st.jobResults = map[string]string{}
		st.jobOrder = nil
		if st.armed {
			st.state = stateArmed
			st.alert = AlertRecord{State: stateArmed, Severity: "info", Summary: "sensor alert armed"}
		} else {
			st.state = stateDisarmed
			st.alert = AlertRecord{State: stateDisarmed, Severity: "info", Summary: "sensor alert disarmed"}
		}
	}
	return &application.ValidateBindingResponse{Valid: true}, nil
}

func (s *Service) HandleEvents(ctx context.Context, events application.ApplicationEventReader, effects application.ApplicationEffectWriter) error {
	if events == nil {
		return status.Errorf(status.CodeInvalidArgument, "nil event reader")
	}
	owner := new(int)
	defer func() {
		s.mu.Lock()
		for id, route := range s.routes {
			if route.owner == owner {
				delete(s.routes, id)
			}
		}
		s.mu.Unlock()
	}()
	for {
		ev, err := events.Recv(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
		if ev == nil || ev.PluginInstanceID == "" || ev.Union == nil {
			continue
		}
		if err := s.attachRoute(ev.PluginInstanceID, effects, owner); err != nil {
			return err
		}
		if err := s.handleEvent(ctx, ev); err != nil {
			return err
		}
	}
}

func (s *Service) attachRoute(instanceID string, writer application.ApplicationEffectWriter, owner *int) error {
	if writer == nil {
		return status.Errorf(status.CodeInvalidArgument, "nil effect writer")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.routes[instanceID]; existing != nil && existing.owner != owner {
		return status.Errorf(status.CodeFailedPrecondition, "instance %q is already attached to another effect stream", instanceID)
	}
	s.routes[instanceID] = &effectRoute{writer: writer, owner: owner}
	return nil
}

func (s *Service) handleEvent(ctx context.Context, ev *application.ApplicationEvent) error {
	if ev == nil || ev.Union == nil || !validInstanceID(ev.PluginInstanceID) {
		return nil
	}
	st, err := s.lookup(ev.PluginInstanceID, false)
	if err != nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if ev.Sequence != 0 {
		if ev.Sequence <= st.lastEnvelopeSeq {
			return nil
		}
		st.lastEnvelopeSeq = ev.Sequence
	}
	s.mu.Lock()
	route := s.routes[st.id]
	s.mu.Unlock()
	if route != nil {
		st.route = route.writer
	}
	switch u := ev.Union.(type) {
	case *application.CapabilityEvent:
		return s.handleCapabilityEventLocked(ctx, st, u)
	case *application.RequestCompleted:
		s.handleCompletionLocked(st, u)
		return nil
	case *application.InstanceLifecycle:
		return nil
	default:
		return nil
	}
}

func (s *Service) handleCapabilityEventLocked(ctx context.Context, st *instanceState, ev *application.CapabilityEvent) error {
	if ev == nil || !st.configured || !st.bindingsValid {
		return nil
	}
	role := ev.RequirementID
	entity := st.binding(role)
	if entity == "" || ev.EntityID != entity {
		return nil
	}
	if ev.EventType == PropertyObservedEvent {
		obs, ok := parseObservation(ev, role, entity)
		if !ok {
			return nil
		}
		ss := st.sensor(role)
		if !ss.accept(obs, s.now().UTC()) {
			return nil
		}
		if !obs.usable {
			return nil
		}
		return s.evaluateObservationLocked(ctx, st, role, obs.value)
	}
	if eventIsTrigger(role, ev.EventType) {
		if role == ContactRequirement && !st.config.ContactEnabled {
			return nil
		}
		if role == VibrationRequirement && !st.config.VibrationEnabled {
			return nil
		}
		return s.triggerLocked(ctx, st, role, role, eventValue(true), nil, triggerSummary(role, 1, nil, "event"))
	}
	if eventIsRecovery(role, ev.EventType) {
		return s.recoverLocked(ctx, st, role, eventValue(false))
	}
	return nil
}

func (s *Service) evaluateObservationLocked(ctx context.Context, st *instanceState, role string, value float64) error {
	if !st.armed {
		return nil
	}
	switch role {
	case TemperatureRequirement:
		if value < st.config.TemperatureMin {
			threshold := st.config.TemperatureMin
			return s.triggerLocked(ctx, st, role+"-low", role, value, &threshold, triggerSummary(role, value, &threshold, "low"))
		}
		if value > st.config.TemperatureMax {
			threshold := st.config.TemperatureMax
			return s.triggerLocked(ctx, st, role+"-high", role, value, &threshold, triggerSummary(role, value, &threshold, "high"))
		}
		return s.recoverLocked(ctx, st, role, value)
	case IlluminanceRequirement:
		if st.config.LightMin == nil && st.config.LightMax == nil {
			return nil
		}
		if st.config.LightMin != nil && value < *st.config.LightMin {
			threshold := *st.config.LightMin
			return s.triggerLocked(ctx, st, role+"-low", role, value, &threshold, triggerSummary(role, value, &threshold, "low"))
		}
		if st.config.LightMax != nil && value > *st.config.LightMax {
			threshold := *st.config.LightMax
			return s.triggerLocked(ctx, st, role+"-high", role, value, &threshold, triggerSummary(role, value, &threshold, "high"))
		}
		return s.recoverLocked(ctx, st, role, value)
	case ContactRequirement:
		if !st.config.ContactEnabled {
			return nil
		}
		if value == 1 {
			return s.triggerLocked(ctx, st, role, role, value, nil, triggerSummary(role, value, nil, "state"))
		}
		if value == 0 {
			return s.recoverLocked(ctx, st, role, value)
		}
	case VibrationRequirement:
		if !st.config.VibrationEnabled {
			return nil
		}
		if value == 1 {
			return s.triggerLocked(ctx, st, role, role, value, nil, triggerSummary(role, value, nil, "state"))
		}
		if value == 0 {
			return s.recoverLocked(ctx, st, role, value)
		}
	}
	return nil
}

func (s *Service) triggerLocked(ctx context.Context, st *instanceState, key, sensor string, value float64, threshold *float64, summary string) error {
	if !st.armed {
		return nil
	}
	if _, exists := st.active[key]; exists {
		return nil
	}
	now := s.now().UTC()
	if previous, ok := st.lastTriggered[key]; ok && !previous.IsZero() {
		if now.Before(previous) || now.Sub(previous) < time.Duration(st.config.CooldownS)*time.Second {
			return nil
		}
	}
	active := activeAlert{Key: key, Sensor: sensor, Value: value, Threshold: threshold, Severity: severityFor(sensor), Summary: summary}
	st.active[key] = active
	st.lastTriggered[key] = now
	st.setState(stateTriggered, now, &active)
	if err := s.emitAlertLocked(ctx, st); err != nil {
		return err
	}
	if err := s.emitSoundLocked(ctx, st, key); err != nil {
		return err
	}
	return s.emitLightLocked(ctx, st, st.config.AlertLEDMask, key)
}

func (s *Service) recoverLocked(ctx context.Context, st *instanceState, sensor string, value float64) error {
	removed := false
	for key, active := range st.active {
		if active.Sensor == sensor {
			delete(st.active, key)
			removed = true
		}
	}
	if !removed || len(st.active) != 0 {
		return nil
	}
	now := s.now().UTC()
	if st.alert.Value != nil {
		*st.alert.Value = value
	}
	st.setState(stateRecovered, now, nil)
	if err := s.emitAlertLocked(ctx, st); err != nil {
		return err
	}
	return s.emitLightLocked(ctx, st, 0, sensor+"-recovered")
}

func (s *Service) emitAlertLocked(ctx context.Context, st *instanceState) error {
	return s.sendEffectLocked(ctx, st, domainRecord(alertRecordType, alertRecordID, st.alertRecord()))
}

func (s *Service) emitSoundLocked(ctx context.Context, st *instanceState, key string) error {
	if st.config.Silent || st.config.AlertTone == nil || !st.isBound(SoundRequirement) {
		return nil
	}
	args := jsonText(st.config.AlertTone)
	return s.requestCommandLocked(ctx, st, st.binding(SoundRequirement), actionTone, args, key)
}

func (s *Service) emitLightLocked(ctx context.Context, st *instanceState, mask int, key string) error {
	if !st.isBound(LightRequirement) {
		return nil
	}
	args := jsonText(map[string]int{"mask": mask})
	return s.requestCommandLocked(ctx, st, st.binding(LightRequirement), actionLED, args, key)
}

func (s *Service) requestCommandLocked(ctx context.Context, st *instanceState, entity, action, args, condition string) error {
	if st.route == nil {
		return status.Errorf(status.CodeUnavailable, "no active effect stream for this instance")
	}
	st.commandSeq++
	key := commandKey(st.id, condition, st.commandSeq)
	st.pending[key] = commandResult{RequestID: key, EntityID: entity, Action: action, State: "pending"}
	return s.sendEffectLocked(ctx, st, &application.RequestCommand{
		EntityID: entity, Action: action, ArgsJSON: args, IdempotencyKey: key,
		Deadline: s.now().UTC().Add(30 * time.Second).Format(time.RFC3339Nano),
	})
}

func commandKey(instanceID, condition string, sequence uint64) string {
	sum := sha256.Sum256([]byte(instanceID))
	prefix := hex.EncodeToString(sum[:])[:12]
	condition = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, condition)
	return fmt.Sprintf("sensor-alert-%s-%d-%s", prefix, sequence, condition)
}

func (s *Service) handleCompletionLocked(st *instanceState, completed *application.RequestCompleted) {
	if completed == nil {
		return
	}
	state, terminal := terminalCommandState(completed.State)
	if !terminal {
		return
	}
	pending, ok := st.pending[completed.RequestID]
	if !ok || pending.EntityID != completed.EntityID || pending.Action != completed.Action {
		return
	}
	result := commandResult{
		RequestID: completed.RequestID, EntityID: completed.EntityID, Action: completed.Action,
		State: state, ErrorCode: completed.ErrorCode,
	}
	delete(st.pending, completed.RequestID)
	st.lastCommand = &result
}

func terminalCommandState(state application.CommandState) (string, bool) {
	switch state {
	case application.CommandStateSucceeded:
		return "succeeded", true
	case application.CommandStateFailed:
		return "failed", true
	case application.CommandStateTimedOut:
		return "timedout", true
	case application.CommandStateCancelled:
		return "cancelled", true
	default:
		return "", false
	}
}

func (s *Service) sendEffectLocked(ctx context.Context, st *instanceState, effect application.ApplicationEffectUnion) error {
	if st.route == nil {
		return status.Errorf(status.CodeUnavailable, "no active effect stream for this instance")
	}
	st.effectSeq++
	return st.route.Send(ctx, &application.ApplicationEffect{
		PluginInstanceID: st.id, Sequence: st.effectSeq, SchemaVersion: application.SchemaVersion, Union: effect,
	})
}

func (s *Service) RunJob(ctx context.Context, req *application.RunJobRequest) (*application.RunJobResponse, error) {
	if req == nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "nil job request")
	}
	switch req.JobID {
	case jobArm, jobDisarm, jobStatus, jobCheckFreshness:
	default:
		return nil, status.Errorf(status.CodeUnimplemented, "unknown job %q", req.JobID)
	}
	if req.JobID == jobArm || req.JobID == jobDisarm || req.JobID == jobStatus {
		if req.JobType == "scheduled" {
			return nil, status.Errorf(status.CodeFailedPrecondition, "job %q is manual-only", req.JobID)
		}
	}
	if len(req.IdempotencyKey) > 256 {
		return nil, status.Errorf(status.CodeInvalidArgument, "idempotency key exceeds 256 bytes")
	}
	if err := validateEmptyObject(req.ArgsJSON); err != nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "%v", err)
	}
	if req.Deadline != "" {
		deadline, err := time.Parse(time.RFC3339Nano, req.Deadline)
		if err != nil {
			return nil, status.Errorf(status.CodeInvalidArgument, "invalid job deadline")
		}
		if !s.now().Before(deadline) {
			return nil, status.Errorf(status.CodeDeadlineExceeded, "job deadline expired")
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	st, err := s.lookup(req.PluginInstanceID, false)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.configured {
		return nil, status.Errorf(status.CodeFailedPrecondition, "instance is not configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	jobKey := req.JobID + "\x00" + req.IdempotencyKey
	if req.IdempotencyKey != "" {
		if result, ok := st.jobResults[jobKey]; ok {
			return &application.RunJobResponse{JobID: req.JobID, Status: status.New(), ResultJSON: result}, nil
		}
	}
	var result string
	switch req.JobID {
	case jobArm:
		changed, err := s.transitionLocked(ctx, st, true)
		if err != nil {
			return nil, err
		}
		result = jsonText(map[string]any{"state": st.state, "armed": st.armed, "changed": changed, "alert": st.alertRecord()})
	case jobDisarm:
		changed, err := s.transitionLocked(ctx, st, false)
		if err != nil {
			return nil, err
		}
		result = jsonText(map[string]any{"state": st.state, "armed": st.armed, "changed": changed, "alert": st.alertRecord()})
	case jobStatus:
		result = jsonText(s.statusLocked(st))
	case jobCheckFreshness:
		result = jsonText(s.freshnessLocked(st))
	}
	if req.IdempotencyKey != "" {
		if len(st.jobOrder) == maxJobResults {
			delete(st.jobResults, st.jobOrder[0])
			st.jobOrder = st.jobOrder[1:]
		}
		st.jobOrder = append(st.jobOrder, jobKey)
		st.jobResults[jobKey] = result
	}
	return &application.RunJobResponse{JobID: req.JobID, Status: status.New(), ResultJSON: result}, nil
}

func (s *Service) transitionLocked(ctx context.Context, st *instanceState, armed bool) (bool, error) {
	if st.armed == armed && st.state == map[bool]string{true: stateArmed, false: stateDisarmed}[armed] {
		return false, nil
	}
	st.armed = armed
	st.active = map[string]activeAlert{}
	if armed {
		st.state = stateArmed
		st.alert = AlertRecord{State: stateArmed, Severity: "info", Summary: "sensor alert armed"}
		if err := s.emitAlertLocked(ctx, st); err != nil {
			return false, err
		}
		return true, nil
	}
	st.state = stateDisarmed
	st.alert = AlertRecord{State: stateDisarmed, Severity: "info", Summary: "sensor alert disarmed"}
	if err := s.emitAlertLocked(ctx, st); err != nil {
		return false, err
	}
	if err := s.emitLightLocked(ctx, st, 0, "disarm"); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) statusLocked(st *instanceState) map[string]any {
	pending := make([]commandResult, 0, len(st.pending))
	for _, value := range st.pending {
		pending = append(pending, value)
	}
	return map[string]any{
		"instance_id": st.id, "configured": st.configured, "bindings_valid": st.bindingsValid,
		"bindings": st.bindings, "armed": st.armed, "state": st.state, "alert": st.alertRecord(),
		"pending_commands": pending, "last_command": st.lastCommand,
	}
}

type freshnessSensor struct {
	Sensor         string  `json:"sensor"`
	EntityID       string  `json:"entity_id"`
	LastObservedAt string  `json:"last_observed_at,omitempty"`
	AgeS           float64 `json:"age_s"`
	Fresh          bool    `json:"fresh"`
}

func (s *Service) freshnessLocked(st *instanceState) map[string]any {
	now := s.now().UTC()
	out := make([]freshnessSensor, 0, 4)
	for _, role := range []string{TemperatureRequirement, IlluminanceRequirement, ContactRequirement, VibrationRequirement} {
		entity := st.binding(role)
		if entity == "" {
			continue
		}
		ss := st.sensor(role)
		item := freshnessSensor{Sensor: role, EntityID: entity}
		if !ss.lastSeenAt.IsZero() {
			item.LastObservedAt = ss.lastSeenAt.UTC().Format(time.RFC3339Nano)
			item.AgeS = now.Sub(ss.lastSeenAt).Seconds()
			item.Fresh = item.AgeS <= freshnessWindow.Seconds()
		}
		out = append(out, item)
	}
	return map[string]any{"instance_id": st.id, "checked_at": now, "window_s": int(freshnessWindow.Seconds()), "sensors": out}
}

func (s *Service) HandleRequest(_ context.Context, req *application.PluginHTTPRequest) (*application.PluginHTTPResponse, error) {
	if req == nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "nil HTTP request")
	}
	response := func(code uint32, value any) *application.PluginHTTPResponse {
		return &application.PluginHTTPResponse{StatusCode: code, Headers: map[string]string{"content-type": "application/json; charset=utf-8", "cache-control": "no-store"}, Body: []byte(jsonText(value))}
	}
	if req.Method != "GET" {
		r := response(405, map[string]string{"error": "read_only"})
		r.Headers["allow"] = "GET"
		return r, nil
	}
	if req.Path != "" && req.Path != "/" && req.Path != "/status" {
		return response(404, map[string]string{"error": "not_found"}), nil
	}
	if req.Context.InstanceID == "" || (req.PluginInstanceID != "" && req.PluginInstanceID != req.Context.InstanceID) {
		return nil, status.Errorf(status.CodeInvalidArgument, "host instance context is missing or mismatched")
	}
	st, err := s.lookup(req.Context.InstanceID, false)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return response(200, s.statusLocked(st)), nil
}

func (s *Service) Health(context.Context) (*application.HealthResponse, error) {
	serving := application.HealthStateServing
	if !s.initialized.Load() || s.closed.Load() {
		serving = application.HealthStateNotServing
	}
	s.mu.Lock()
	instances := make([]*instanceState, 0, len(s.instances))
	for _, st := range s.instances {
		instances = append(instances, st)
	}
	s.mu.Unlock()
	result := &application.HealthResponse{State: serving}
	for _, st := range instances {
		st.mu.Lock()
		state := serving
		if !st.configured || !st.bindingsValid {
			state = application.HealthStateNotServing
		}
		result.Instances = append(result.Instances, application.InstanceHealth{PluginInstanceID: st.id, State: state, Detail: "alert_state=" + st.state})
		st.mu.Unlock()
	}
	return result, nil
}

func (s *Service) Shutdown(context.Context, *application.ShutdownRequest) (*application.ShutdownResponse, error) {
	s.closed.Store(true)
	return &application.ShutdownResponse{Status: status.New()}, nil
}
