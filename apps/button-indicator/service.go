// Package buttonindicator is the Button Indicator reference application: the
// default walking light and opt-in service-call workflow use only generic
// key, LED and optional buzzer capabilities. A declarative heartbeat is driven
// by the Core Durable Scheduler in both modes. The application has no knowledge
// of any hardware, Driver id, port or vendor field.
package buttonindicator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/status"
)

// Manifest identity. These must mirror plugin.yaml.
const (
	pluginIDValue = "io.github.deliciousbuding.cloud-path-app-button-indicator"
	pluginVersion = "0.2.1"

	jobBootstrap  = "bootstrap"
	jobHeartbeat  = "indicator-heartbeat"
	heartbeatCaps = "indicator-heartbeat"

	ledAction = "led"
	buzzerAct = "buzzer"

	keyCap   = "cloudpath.dev/capability/key@1"
	ledCap   = "cloudpath.dev/capability/led@1"
	buzzerCp = "cloudpath.dev/capability/buzzer@1"
)

// keyPressEvent is the event type delivered by the key@1 capability. The
// interpretation "a press advances the walking light" lives here, in the
// application, and never in a Driver.
const keyPressEvent = keyCap + "/press"

// instanceState is the per-plugin-instance runtime state.
type instanceState struct {
	// callMu serializes call transitions and their effects, including jobs,
	// completions and result deadlines. Field access still uses Service.mu.
	callMu    sync.Mutex
	calls     *callRuntime
	config    *Config
	configRev uint32
	bindings  map[string][]string // requirement id -> entity ids
	// heartbeatRev is the config revision the declarative heartbeat was last
	// registered for. bootstrap re-declares only when the revision changes.
	heartbeatRev uint32
	heartbeatOn  bool
	pressCount   int64
	ledMask      int
	lastSeq      uint64
	jobs         map[string]string // idempotency key -> result JSON
}

// Service implements the ApplicationService protocol for Button Indicator.
type Service struct {
	pluginID  string
	version   string
	runtimeID string

	// sendMu keeps effect sequence allocation and transmission ordered when
	// a command completion/deadline races a job or heartbeat.
	sendMu      sync.Mutex
	mu          sync.Mutex
	initialized bool
	closed      bool
	// writers 按实例路由 effect：共享进程里每个 App 实例有独立的 RPC 会话
	// （AppHost 的 appruntime 每实例一条 HandleEvents 流），effect 必须回到
	// 发起事件的那条流，否则对端按「实例不匹配」拒绝。
	writers   map[string]application.ApplicationEffectWriter
	effectSeq uint64
	now       func() time.Time
	instances map[string]*instanceState
}

var _ application.ApplicationServer = (*Service)(nil)

// ApplicationID returns the manifest application id.
func ApplicationID() string { return pluginIDValue }

// Version returns the manifest application version.
func Version() string { return pluginVersion }

// New returns a fresh, uninitialized Button Indicator service.
func New() *Service {
	return &Service{
		pluginID:  pluginIDValue,
		version:   pluginVersion,
		now:       time.Now,
		instances: map[string]*instanceState{},
	}
}

// ---------------------------------------------------------------------------
// Lifecycle / handshake
// ---------------------------------------------------------------------------

// Initialize negotiates the application protocol version and pins a runtime id.
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
		s.runtimeID = fmt.Sprintf("button-indicator-%d", time.Now().UnixNano())
	}
	runtimeID := s.runtimeID
	s.mu.Unlock()

	return &application.InitializeResponse{
		NegotiatedProtocolVersion: negotiated,
		Status:                    status.New(),
		RuntimeID:                 runtimeID,
	}, nil
}

// Describe reports capability requirements and jobs. bootstrap remains an
// automatically driven descriptor job and registers the heartbeat per config
// revision. The service-call actions are ManualOnly (Core v0.2.15+).
// indicator-heartbeat is deliberately absent: only the cron scheduler owns it.
func (s *Service) Describe(context.Context) (*application.ApplicationDescriptor, error) {
	return &application.ApplicationDescriptor{
		ApplicationID:  s.pluginID,
		Version:        s.version,
		SchemaVersions: []string{application.SchemaVersion},
		Requirements: []application.RequirementDescriptor{
			{ID: "button-input", Capability: keyCap, Cardinality: "one-or-more", MinItems: 1},
			{ID: "indicator", Capability: ledCap, Cardinality: "one"},
			{ID: "sound", Capability: buzzerCp, Cardinality: "zero-or-one"},
			{ID: "acknowledge-input", Capability: keyCap, Cardinality: "zero-or-one"},
		},
		Jobs: []application.JobDescriptor{
			{ID: jobBootstrap, Title: "Register declarative schedules", InputSchemaJSON: "{}"},
			{ID: jobAcknowledgePending, Title: "确认并解除提示", InputSchemaJSON: acknowledgePendingJobSchema, ManualOnly: true},
			{ID: jobRequest, Title: "远程代工位发起呼叫（备用）", InputSchemaJSON: requestJobSchema, ManualOnly: true},
			{ID: jobAcknowledge, Title: "按编号确认其它呼叫（精确）", InputSchemaJSON: acknowledgeJobSchema, ManualOnly: true},
		},
		DeclarativeOnly: false,
	}, nil
}

// ConfigureInstance parses and validates the bounded config for one instance.
func (s *Service) ConfigureInstance(_ context.Context, req *application.ConfigureInstanceRequest) (*application.ConfigureInstanceResponse, error) {
	if req == nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "nil configure request")
	}
	cfg, err := UnmarshalConfig(req.Config)
	if err != nil {
		return &application.ConfigureInstanceResponse{
			PluginInstanceID: req.PluginInstanceID,
			AppliedRevision:  0,
			Status:           status.Errorf(status.CodeInvalidArgument, "%v", err),
		}, nil
	}

	s.mu.Lock()
	st := s.instance(req.PluginInstanceID)
	if st.config != nil && st.config.ResolvedMode() != cfg.ResolvedMode() && st.callBusy() {
		s.mu.Unlock()
		return &application.ConfigureInstanceResponse{
			PluginInstanceID: req.PluginInstanceID,
			Status:           status.Errorf(status.CodeFailedPrecondition, "acknowledge the pending call and wait for command results before changing mode"),
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

// ValidateBinding checks bindings against the declared requirements and, when
// valid, stores them for event processing.
func (s *Service) ValidateBinding(_ context.Context, req *application.ValidateBindingRequest) (*application.ValidateBindingResponse, error) {
	if req == nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "nil validate request")
	}
	issues := validateBindings(req.Bindings)
	valid := len(issues) == 0
	if valid {
		s.mu.Lock()
		st := s.instance(req.PluginInstanceID)
		if st.callBusy() && !sameBindings(st.bindings, req.Bindings) {
			valid = false
			issues = append(issues, application.BindingIssue{
				Severity: "error", Message: "acknowledge the pending call and wait for command results before rebinding",
			})
		} else {
			st.bindings = groupBindings(req.Bindings)
		}
		s.mu.Unlock()
	}
	return &application.ValidateBindingResponse{Valid: valid, Issues: issues}, nil
}

// ---------------------------------------------------------------------------
// Event stream
// ---------------------------------------------------------------------------

// HandleEvents is the bidi event/effect stream.
func (s *Service) HandleEvents(ctx context.Context, events application.ApplicationEventReader, effects application.ApplicationEffectWriter) error {
	if events == nil {
		return status.Errorf(status.CodeInvalidArgument, "nil event reader")
	}
	defer func() {
		s.mu.Lock()
		for id, w := range s.writers {
			if w == effects {
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
	instanceID := ev.PluginInstanceID

	s.mu.Lock()
	st := s.instance(instanceID)
	if ev.Sequence != 0 {
		if ev.Sequence <= st.lastSeq {
			s.mu.Unlock()
			return nil // duplicate event; idempotent
		}
		st.lastSeq = ev.Sequence
	}
	s.mu.Unlock()

	switch u := ev.Union.(type) {
	case *application.CapabilityEvent:
		return s.onCapabilityEvent(instanceID, u)
	case *application.RequestCompleted:
		return s.onCallCommandCompleted(instanceID, u)
	case *application.ScheduleTick:
		return nil // button-indicator declares no schedule windows
	case *application.InstanceLifecycle:
		return s.flushCallRecords(instanceID)
	default:
		return nil
	}
}

func (s *Service) onCapabilityEvent(instanceID string, ev *application.CapabilityEvent) error {
	if ev == nil {
		return nil
	}
	switch ev.EventType {
	case keyPressEvent:
		return s.onKeyEvent(instanceID, ev)
	default:
		return nil
	}
}

// onKeyEvent interprets a key press on a bound button entity as "advance the
// walking light by one LED and persist the press". The business meaning of
// the generic key@1 event lives entirely here.
func (s *Service) onKeyEvent(instanceID string, ev *application.CapabilityEvent) error {
	s.mu.Lock()
	st := s.instance(instanceID)
	if st.config == nil {
		s.mu.Unlock()
		return nil // not configured yet
	}
	if st.config.ResolvedMode() == modeServiceCall {
		s.mu.Unlock()
		return s.onServiceCallKey(instanceID, ev)
	}
	if !st.bound("button-input", ev.EntityID) {
		s.mu.Unlock()
		return nil // event for an entity this instance did not bind
	}
	st.pressCount++
	st.ledMask = 1 << int((st.pressCount-1)%8)
	count, mask := st.pressCount, st.ledMask
	entity := ev.EntityID
	led := firstEntity(st.bindings, "indicator")
	sound := firstEntity(st.bindings, "sound")
	beep := st.config.BeepOnPress && sound != ""
	at := s.now().UTC()
	effects := []application.ApplicationEffectUnion{
		&application.UpsertDomainRecord{
			RecordType: "press",
			RecordID:   "last",
			DataJSON: mustJSON(map[string]any{
				"count":      count,
				"entity":     entity,
				"mask":       mask,
				"pressed_at": at.Format(time.RFC3339),
			}),
			Version: fmt.Sprintf("%d", count),
		},
	}
	if led != "" {
		effects = append(effects, &application.RequestCommand{
			EntityID:       led,
			Action:         ledAction,
			ArgsJSON:       mustJSON(map[string]int{"mask": mask}),
			IdempotencyKey: fmt.Sprintf("led-%d", count),
		})
	}
	if beep {
		effects = append(effects, &application.RequestCommand{
			EntityID:       sound,
			Action:         buzzerAct,
			ArgsJSON:       mustJSON(map[string]int{"freq": 1, "duration": 1}),
			IdempotencyKey: fmt.Sprintf("beep-%d", count),
		})
	}
	s.mu.Unlock()

	return s.flush(instanceID, effects)
}

// ---------------------------------------------------------------------------
// Jobs
// ---------------------------------------------------------------------------

// RunJob implements the heartbeat jobs and explicit service-call actions. bootstrap (driven every minute by the
// AppHost descriptor-job loop) registers the declarative heartbeat through a
// schedule_task effect, idempotent per config revision. indicator-heartbeat
// is dispatched by the Core Durable Scheduler on the cron schedule and writes
// the heartbeat domain record.
func (s *Service) RunJob(_ context.Context, req *application.RunJobRequest) (*application.RunJobResponse, error) {
	if req == nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "nil job request")
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, status.Errorf(status.CodeUnavailable, "plugin is shutting down")
	}
	st := s.instance(req.PluginInstanceID)
	if st.config == nil {
		s.mu.Unlock()
		return nil, status.Errorf(status.CodeFailedPrecondition, "instance %q is not configured", req.PluginInstanceID)
	}

	switch req.JobID {
	case jobRequest, jobAcknowledge, jobAcknowledgePending:
		s.mu.Unlock()
		return s.runCallJob(req)
	case jobBootstrap:
		if req.IdempotencyKey != "" {
			if _, done := st.jobs[req.IdempotencyKey]; done {
				s.mu.Unlock()
				return &application.RunJobResponse{JobID: req.JobID, Status: status.New()}, nil
			}
		}
		// 声明只在 effect writer 就绪时标记成功：流刚开而首事件未达时
		// sendEffect 会静默丢弃（Core v0.2.9 起流开启即派发初始 lifecycle
		// 事件，此处是对旧核的防御——下个分钟 tick 自然重试）。
		fresh := !st.heartbeatOn || st.heartbeatRev != st.configRev
		hasWriter := s.writers[req.PluginInstanceID] != nil
		var effects []application.ApplicationEffectUnion
		if fresh && hasWriter {
			effects = append(effects, &application.ScheduleTask{
				ScheduleID:  heartbeatCaps,
				Cron:        st.config.ResolvedHeartbeatCron(),
				PayloadJSON: "{}",
			})
			st.heartbeatOn = true
			st.heartbeatRev = st.configRev
		}
		if req.IdempotencyKey != "" {
			if st.jobs == nil {
				st.jobs = map[string]string{}
			}
			st.jobs[req.IdempotencyKey] = "{}"
		}
		s.mu.Unlock()
		if err := s.flush(req.PluginInstanceID, effects); err != nil {
			return nil, err
		}
		return &application.RunJobResponse{JobID: req.JobID, Status: status.New()}, nil

	case jobHeartbeat:
		if req.IdempotencyKey != "" {
			if _, done := st.jobs[req.IdempotencyKey]; done {
				s.mu.Unlock()
				return &application.RunJobResponse{JobID: req.JobID, Status: status.New()}, nil
			}
		}
		count, mask := st.pressCount, st.ledMask
		at := s.now().UTC().Format(time.RFC3339)
		heartbeat := map[string]any{"at": at, "press_count": count, "led_mask": mask}
		if st.config.ResolvedMode() == modeServiceCall {
			// A desired command mask is not an observed hardware state.
			delete(heartbeat, "led_mask")
			delete(heartbeat, "press_count")
			heartbeat["mode"] = modeServiceCall
			st.addCallSummary(heartbeat)
		}
		dataJSON := mustJSON(heartbeat)
		if req.IdempotencyKey != "" {
			if st.jobs == nil {
				st.jobs = map[string]string{}
			}
			st.jobs[req.IdempotencyKey] = "{}"
		}
		s.mu.Unlock()
		if err := s.sendEffect(req.PluginInstanceID, &application.UpsertDomainRecord{
			RecordType: "heartbeat",
			RecordID:   "last",
			DataJSON:   dataJSON,
			Version:    at,
		}); err != nil {
			return nil, err
		}
		return &application.RunJobResponse{JobID: req.JobID, Status: status.New()}, nil

	default:
		s.mu.Unlock()
		return nil, status.Errorf(status.CodeUnimplemented, "job %q is not implemented", req.JobID)
	}
}

// ---------------------------------------------------------------------------
// HTTP subroute / health / shutdown
// ---------------------------------------------------------------------------

// HandleRequest serves the plugin-scoped HTTP subroute: a read-only summary.
func (s *Service) HandleRequest(_ context.Context, req *application.PluginHTTPRequest) (*application.PluginHTTPResponse, error) {
	if req == nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "nil http request")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, status.Errorf(status.CodeUnavailable, "plugin is shutting down")
	}
	st := s.instance(req.Context.InstanceID)
	code := uint32(200)
	body := []byte("{}")
	if req.Method == "GET" {
		statusBody := map[string]any{
			"instance_id": req.Context.InstanceID,
		}
		if st.config != nil {
			statusBody["heartbeat_cron"] = st.config.ResolvedHeartbeatCron()
			statusBody["heartbeat_registered"] = st.heartbeatOn
			statusBody["mode"] = st.config.ResolvedMode()
			if st.config.ResolvedMode() == modeServiceCall {
				st.addCallSummary(statusBody)
			} else {
				statusBody["press_count"] = st.pressCount
				statusBody["led_mask"] = st.ledMask
			}
			statusBody["buttons"] = len(st.bindings["button-input"])
		} else {
			statusBody["configured"] = false
		}
		body, _ = json.Marshal(statusBody)
	} else {
		code = 405
	}
	s.mu.Unlock()

	return &application.PluginHTTPResponse{
		StatusCode: code,
		Headers:    map[string]string{"content-type": "application/json"},
		Body:       body,
	}, nil
}

// Health reports readiness.
func (s *Service) Health(context.Context) (*application.HealthResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := application.HealthStateServing
	if s.closed {
		state = application.HealthStateNotServing
	}
	insts := make([]application.InstanceHealth, 0, len(s.instances))
	for id := range s.instances {
		insts = append(insts, application.InstanceHealth{PluginInstanceID: id, State: state})
	}
	return &application.HealthResponse{State: state, Instances: insts}, nil
}

// Shutdown marks the service closed; the process exit is handled by the
// shutdownAwareService wrapper in cmd/.
func (s *Service) Shutdown(_ context.Context, _ *application.ShutdownRequest) (*application.ShutdownResponse, error) {
	s.mu.Lock()
	s.closed = true
	for _, st := range s.instances {
		if st.calls != nil {
			for _, cmd := range st.calls.commands {
				if cmd.timer != nil {
					cmd.timer.Stop()
				}
			}
		}
	}
	s.mu.Unlock()
	return &application.ShutdownResponse{Status: status.New()}, nil
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

func (s *Service) instance(id string) *instanceState {
	st, ok := s.instances[id]
	if !ok {
		st = &instanceState{}
		s.instances[id] = st
	}
	return st
}

// bound reports whether the entity is bound to the requirement in this
// instance. The authoritative cross-tenant check happened in Core (Binder);
// this is the event-routing guard.
func (st *instanceState) bound(requirementID, entityID string) bool {
	if entityID == "" {
		return false
	}
	for _, e := range st.bindings[requirementID] {
		if e == entityID {
			return true
		}
	}
	return false
}

func firstEntity(bindings map[string][]string, requirementID string) string {
	if list := bindings[requirementID]; len(list) > 0 {
		return list[0]
	}
	return ""
}

func (s *Service) flush(instanceID string, effects []application.ApplicationEffectUnion) error {
	for _, u := range effects {
		if err := s.sendEffect(instanceID, u); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) sendEffect(instanceID string, union application.ApplicationEffectUnion) error {
	return s.emitEffect(instanceID, union, false)
}

func (s *Service) emitEffect(instanceID string, union application.ApplicationEffectUnion, requireWriter bool) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
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
		if requireWriter {
			return status.Errorf(status.CodeUnavailable, "instance %q has no active effect stream", instanceID)
		}
		return nil // preserve legacy fire-and-forget behavior
	}
	eff := &application.ApplicationEffect{
		PluginInstanceID: instanceID,
		Sequence:         seq,
		SchemaVersion:    application.SchemaVersion,
		Union:            union,
	}
	if requireWriter {
		ctx, cancel := context.WithTimeout(context.Background(), callResultWait)
		defer cancel()
		return writer.Send(ctx, eff)
	}
	return writer.Send(context.Background(), eff)
}

// validateBindings enforces the declared requirement cardinalities and rejects
// any requirement id that is not part of this application (which structurally
// rules out Driver coupling).
func validateBindings(bindings []application.Binding) []application.BindingIssue {
	allowed := map[string]struct{}{"button-input": {}, "indicator": {}, "sound": {}, "acknowledge-input": {}}
	var issues []application.BindingIssue
	seen := map[string]bool{}

	for _, b := range bindings {
		if _, ok := allowed[b.RequirementID]; !ok {
			issues = append(issues, application.BindingIssue{
				RequirementID: b.RequirementID,
				Severity:      "error",
				Message:       fmt.Sprintf("requirement %q is not declared by this application", b.RequirementID),
			})
			continue
		}
		if strings.TrimSpace(b.EntityID) == "" {
			issues = append(issues, application.BindingIssue{
				RequirementID: b.RequirementID,
				Severity:      "error",
				Message:       "entity_id must not be empty",
			})
			continue
		}
		key := b.RequirementID + "\x00" + b.EntityID
		if seen[key] {
			issues = append(issues, application.BindingIssue{
				RequirementID: b.RequirementID,
				Severity:      "error",
				Message:       fmt.Sprintf("duplicate entity %q for requirement %q", b.EntityID, b.RequirementID),
			})
			continue
		}
		seen[key] = true
	}

	counts := map[string]int{}
	for _, b := range bindings {
		if _, ok := allowed[b.RequirementID]; ok && strings.TrimSpace(b.EntityID) != "" {
			counts[b.RequirementID]++
		}
	}
	if counts["button-input"] < 1 {
		issues = append(issues, application.BindingIssue{
			RequirementID: "button-input",
			Severity:      "error",
			Message:       "at least one button entity is required",
		})
	}
	if counts["indicator"] != 1 {
		issues = append(issues, application.BindingIssue{
			RequirementID: "indicator",
			Severity:      "error",
			Message:       "exactly one indicator entity is required",
		})
	}
	if counts["sound"] > 1 {
		issues = append(issues, application.BindingIssue{
			RequirementID: "sound",
			Severity:      "error",
			Message:       "at most one sound entity is allowed",
		})
	}
	if counts["acknowledge-input"] > 1 {
		issues = append(issues, application.BindingIssue{
			RequirementID: "acknowledge-input", Severity: "error", Message: "at most one acknowledgement button is allowed",
		})
	}
	for _, b := range bindings {
		if b.RequirementID == "acknowledge-input" && seen["button-input\x00"+b.EntityID] {
			issues = append(issues, application.BindingIssue{
				RequirementID: "acknowledge-input", Severity: "error", Message: "request and acknowledgement inputs must be different entities",
			})
		}
	}
	return issues
}

func groupBindings(bindings []application.Binding) map[string][]string {
	out := map[string][]string{}
	for _, b := range bindings {
		if strings.TrimSpace(b.EntityID) == "" {
			continue
		}
		out[b.RequirementID] = append(out[b.RequirementID], b.EntityID)
	}
	return out
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
