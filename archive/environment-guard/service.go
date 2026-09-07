// Package environmentguard is a read-only, device-independent Application plugin.
// It consumes public capability observations and emits only domain records and
// a durable freshness schedule. It never samples devices or requests actions.
package environmentguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/status"
)

const (
	pluginID          = "io.github.deliciousbuding.cloud-path-app-environment-guard"
	pluginVersion     = "0.1.0"
	jobBootstrap      = "bootstrap"
	jobFreshness      = "check-freshness"
	jobRefresh        = "refresh-status"
	emptyObjectSchema = `{"type":"object","properties":{},"additionalProperties":false}`
	maxJobResults     = 128
)

func ApplicationID() string { return pluginID }
func Version() string       { return pluginVersion }

type effectRoute struct {
	mu     sync.Mutex
	writer application.ApplicationEffectWriter
	ctx    context.Context
}

type instanceState struct {
	mu               sync.Mutex
	id               string
	configured       bool
	config           Config
	configRev        uint32
	bindings         map[string]string
	bindingsValid    bool
	sensors          map[string]*sensorState
	route            *effectRoute
	effectSeq        uint64
	scheduleDeclared bool // effect sent, NOT proof of durable execution
	scheduleRevision uint32
	pendingAlerts    map[string]*application.UpsertDomainRecord
	publishedState   string
	publishedContent string
	publishedAt      time.Time
	jobResults       map[string]string
	jobOrder         []string
}

func newInstance(id string) *instanceState {
	return &instanceState{
		id: id, config: DefaultConfig(), bindings: map[string]string{},
		sensors:       map[string]*sensorState{TemperatureRequirement: {}, IlluminanceRequirement: {}},
		pendingAlerts: map[string]*application.UpsertDomainRecord{}, jobResults: map[string]string{},
	}
}

// Service shares a process, not business state or effect writers, across instances.
// All per-instance changes/sends are serialized; another instance never waits on
// this instance's writer while holding the global instance-map lock.
type Service struct {
	mu          sync.Mutex
	instances   map[string]*instanceState
	initialized atomic.Bool
	closed      atomic.Bool
	runtimeID   string
	now         func() time.Time
}

var _ application.ApplicationServer = (*Service)(nil)

func New() *Service { return &Service{instances: map[string]*instanceState{}, now: time.Now} }

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
		s.runtimeID = fmt.Sprintf("environment-guard-%d", s.now().UnixNano())
	}
	runtimeID := s.runtimeID
	s.mu.Unlock()
	s.initialized.Store(true)
	return &application.InitializeResponse{NegotiatedProtocolVersion: application.ProtocolVersion, Status: status.New(), RuntimeID: runtimeID}, nil
}

func (s *Service) Describe(context.Context) (*application.ApplicationDescriptor, error) {
	return &application.ApplicationDescriptor{
		ApplicationID: pluginID, Version: pluginVersion, SchemaVersions: []string{application.SchemaVersion},
		Requirements: []application.RequirementDescriptor{
			{ID: TemperatureRequirement, Capability: TemperatureCapability, Cardinality: "one"},
			{ID: IlluminanceRequirement, Capability: IlluminanceCapability, Cardinality: "one"},
		},
		Jobs: []application.JobDescriptor{
			{ID: jobBootstrap, Title: "注册每分钟新鲜度检查", InputSchemaJSON: emptyObjectSchema},
			{ID: jobRefresh, Title: "重新计算环境状态（不采样）", InputSchemaJSON: emptyObjectSchema, ManualOnly: true},
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

func (s *Service) ConfigureInstance(ctx context.Context, req *application.ConfigureInstanceRequest) (*application.ConfigureInstanceResponse, error) {
	if req == nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "nil configure request")
	}
	cfg, err := UnmarshalConfig(req.Config)
	if err != nil {
		return &application.ConfigureInstanceResponse{PluginInstanceID: req.PluginInstanceID, Status: status.Errorf(status.CodeInvalidArgument, "%v", err)}, nil
	}
	st, err := s.lookup(req.PluginInstanceID, true)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.configured && (req.ConfigRevision < st.configRev || (req.ConfigRevision == st.configRev && !cfg.equal(st.config))) {
		return &application.ConfigureInstanceResponse{PluginInstanceID: req.PluginInstanceID, AppliedRevision: st.configRev, Status: status.Errorf(status.CodeFailedPrecondition, "config revision is stale or reused with different content")}, nil
	}
	changed := !st.configured || st.configRev != req.ConfigRevision
	if changed {
		if !sameThreshold(cfg.LightThreshold, st.config.LightThreshold) || cfg.LightAlertWhen != st.config.LightAlertWhen {
			// Disabling/changing a criterion is not a sensor-proven recovery.
			st.sensors[IlluminanceRequirement].zone = ""
		}
		// A new unit must be backed by a new observation, not relabel old data.
		if cfg.TemperatureUnit != st.config.TemperatureUnit {
			st.sensors[TemperatureRequirement] = &sensorState{}
		}
		if cfg.LightUnit != st.config.LightUnit {
			st.sensors[IlluminanceRequirement] = &sensorState{}
		}
		for _, sensor := range st.sensors {
			sensor.learnedUnit, sensor.hasUnit = "", false
		}
		st.config, st.configRev, st.configured = cfg, req.ConfigRevision, true
		st.scheduleDeclared = false
		st.jobResults, st.jobOrder = map[string]string{}, nil
	}
	if st.route != nil {
		if _, err := s.recompute(ctx, st); err != nil {
			return nil, err
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
		if capability(b.RequirementID) == "" {
			issue(b.RequirementID, "undeclared requirement")
			continue
		}
		if !validInstanceID(b.EntityID) {
			issue(b.RequirementID, "entity_id must be a non-empty trimmed id")
			continue
		}
		if _, exists := out[b.RequirementID]; exists {
			issue(b.RequirementID, "exactly one entity is required; duplicate binding")
			continue
		}
		out[b.RequirementID] = b.EntityID
	}
	for _, role := range roles {
		if out[role] == "" {
			issue(role, "exactly one entity is required")
		}
	}
	return out, issues
}

func (s *Service) ValidateBinding(ctx context.Context, req *application.ValidateBindingRequest) (*application.ValidateBindingResponse, error) {
	if req == nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "nil binding request")
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
	changed := !st.bindingsValid
	for _, role := range roles {
		if st.bindings[role] != bindings[role] {
			st.sensors[role] = &sensorState{}
			changed = true
		}
	}
	st.bindings, st.bindingsValid = bindings, true
	if changed {
		st.jobResults, st.jobOrder = map[string]string{}, nil
	}
	if st.route != nil && st.configured {
		if _, err := s.recompute(ctx, st); err != nil {
			return nil, err
		}
	}
	return &application.ValidateBindingResponse{Valid: true}, nil
}

func nilStream(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}

func (s *Service) HandleEvents(ctx context.Context, events application.ApplicationEventReader, effects application.ApplicationEffectWriter) error {
	if nilStream(events) || nilStream(effects) {
		return status.Errorf(status.CodeInvalidArgument, "event reader and effect writer are required")
	}
	route := &effectRoute{writer: effects, ctx: ctx}
	claimed := map[string]*instanceState{}
	defer func() {
		for _, st := range claimed {
			st.mu.Lock()
			// An older stream's teardown must not erase its replacement writer.
			if st.route == route {
				st.route = nil
				st.scheduleDeclared = false
			}
			st.mu.Unlock()
		}
	}()
	for {
		ev, err := events.Recv(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
		if s.closed.Load() {
			return status.Errorf(status.CodeUnavailable, "plugin is shutting down")
		}
		if ev == nil || ev.Union == nil {
			continue
		}
		if ev.SchemaVersion != "" && ev.SchemaVersion != application.SchemaVersion {
			return status.Errorf(status.CodeInvalidArgument, "unsupported event schema")
		}
		st, err := s.lookup(ev.PluginInstanceID, false)
		if err != nil {
			return err
		}
		st.mu.Lock()
		first := claimed[st.id] == nil
		if first {
			claimed[st.id] = st
			st.route, st.scheduleDeclared = route, false
			for _, sensor := range st.sensors {
				sensor.lastEnvelopeSeq = 0
			}
		} else if st.route != route {
			st.mu.Unlock()
			return status.Errorf(status.CodeAborted, "event stream was superseded")
		}
		changed := false
		if event, ok := ev.Union.(*application.CapabilityEvent); ok && event != nil && st.configured && st.bindingsValid {
			if sensor := st.sensors[event.RequirementID]; sensor != nil {
				changed = sensor.accept(event, ev.Sequence, event.RequirementID, st.bindings[event.RequirementID], s.now().UTC(), st.config)
			}
		}
		if st.configured && (first || changed || len(st.pendingAlerts) > 0) {
			_, err = s.recompute(ctx, st)
		}
		st.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

func (st *instanceState) send(ctx context.Context, effect application.ApplicationEffectUnion) error {
	if st.route == nil || st.route.ctx.Err() != nil {
		return status.Errorf(status.CodeUnavailable, "no active effect stream for this instance")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	st.effectSeq++
	st.route.mu.Lock()
	defer st.route.mu.Unlock()
	return st.route.writer.Send(ctx, &application.ApplicationEffect{PluginInstanceID: st.id, Sequence: st.effectSeq, SchemaVersion: application.SchemaVersion, Union: effect})
}

// ensureSchedule declares a durable job, without running a freshness check.
func (st *instanceState) ensureSchedule(ctx context.Context) error {
	if st.route == nil || st.route.ctx.Err() != nil {
		return status.Errorf(status.CodeUnavailable, "no active effect stream for this instance")
	}
	if !st.scheduleDeclared || st.scheduleRevision != st.configRev {
		if err := st.send(ctx, &application.ScheduleTask{ScheduleID: jobFreshness, Cron: "* * * * *", PayloadJSON: "{}"}); err != nil {
			return err
		}
		st.scheduleDeclared, st.scheduleRevision = true, st.configRev
	}
	return nil
}

// recompute runs under the instance lock. It never changes observation data.
// Outgoing state is acknowledged locally only after Send succeeds; this is not
// an acknowledgement of the Core database. The public stream has no such ACK.
func (s *Service) recompute(ctx context.Context, st *instanceState) (EnvironmentRecord, error) {
	now := s.now().UTC()
	record := st.snapshot(now)
	st.collectTransitions(record)
	if s.closed.Load() {
		return record, status.Errorf(status.CodeUnavailable, "plugin is shutting down")
	}
	if st.route == nil {
		return record, status.Errorf(status.CodeUnavailable, "no active effect stream for this instance")
	}
	if err := st.ensureSchedule(ctx); err != nil {
		return record, err
	}
	state, content := publicationKeys(record)
	if st.publishedState == "" || state != st.publishedState || (content != st.publishedContent && now.Sub(st.publishedAt) >= snapshotInterval) {
		if err := st.send(ctx, domainRecord("environment", "current", record)); err != nil {
			return record, err
		}
		st.publishedAt, st.publishedState, st.publishedContent = now, state, content
	}
	ids := make([]string, 0, len(st.pendingAlerts))
	for id := range st.pendingAlerts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := st.send(ctx, st.pendingAlerts[id]); err != nil {
			return record, err
		}
		delete(st.pendingAlerts, id)
	}
	return record, nil
}

func (s *Service) RunJob(ctx context.Context, req *application.RunJobRequest) (*application.RunJobResponse, error) {
	if req == nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "nil job request")
	}
	switch req.JobID {
	case jobBootstrap, jobFreshness, jobRefresh:
	default:
		return nil, status.Errorf(status.CodeUnimplemented, "unknown job %q", req.JobID)
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
	if len(req.IdempotencyKey) > 256 {
		return nil, status.Errorf(status.CodeInvalidArgument, "idempotency key exceeds 256 bytes")
	}
	args := req.ArgsJSON
	if args == "" {
		args = "{}"
	}
	fields, err := objectFields([]byte(args))
	if err != nil || len(fields) != 0 {
		return nil, status.Errorf(status.CodeInvalidArgument, "job args_json must be an empty JSON object; jobs do not accept measurements")
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
	key := req.JobID + "\x00" + req.IdempotencyKey
	if req.IdempotencyKey != "" {
		if result, ok := st.jobResults[key]; ok {
			return &application.RunJobResponse{JobID: req.JobID, Status: status.New(), ResultJSON: result}, nil
		}
	}
	var result string
	if req.JobID == jobBootstrap {
		// Legacy descriptor minute ticks only (re)declare the durable schedule.
		// They must not also perform its work.
		if err := st.ensureSchedule(ctx); err != nil {
			return nil, err
		}
		result = jsonText(map[string]any{"sampled": false, "freshness_schedule_declared": st.scheduleDeclared})
	} else {
		record, err := s.recompute(ctx, st)
		if err != nil {
			return nil, err
		}
		result = jsonText(map[string]any{"environment": record, "sampled": false, "freshness_schedule_declared": st.scheduleDeclared})
	}
	if req.IdempotencyKey != "" {
		if len(st.jobOrder) == maxJobResults {
			delete(st.jobResults, st.jobOrder[0])
			st.jobOrder = st.jobOrder[1:]
		}
		st.jobOrder = append(st.jobOrder, key)
		st.jobResults[key] = result
	}
	return &application.RunJobResponse{JobID: req.JobID, Status: status.New(), ResultJSON: result}, nil
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
	return response(200, map[string]any{
		"instance_id": st.id, "environment": st.snapshot(s.now().UTC()), "sampled": false,
		"effect_stream_connected":     st.route != nil && st.route.ctx.Err() == nil,
		"freshness_schedule_declared": st.scheduleDeclared,
	}), nil
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
	sort.Slice(instances, func(i, j int) bool { return instances[i].id < instances[j].id })
	result := &application.HealthResponse{State: serving}
	for _, st := range instances {
		st.mu.Lock()
		state := serving
		if !st.configured || !st.bindingsValid || st.route == nil || st.route.ctx.Err() != nil {
			state = application.HealthStateNotServing
		}
		result.Instances = append(result.Instances, application.InstanceHealth{PluginInstanceID: st.id, State: state, Detail: "environment_status=" + st.snapshot(s.now().UTC()).Status})
		st.mu.Unlock()
	}
	return result, nil
}

func (s *Service) Shutdown(context.Context, *application.ShutdownRequest) (*application.ShutdownResponse, error) {
	s.closed.Store(true)
	return &application.ShutdownResponse{Status: status.New()}, nil
}
