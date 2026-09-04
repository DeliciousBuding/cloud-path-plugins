package scheduledcompartment

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

// Manifest identity of the reference application. These must mirror
// examples/scheduled-compartment/plugin.yaml.
const (
	pluginIDValue   = "io.github.deliciousbuding.cloud-path-app-scheduled-compartment"
	pluginVersion   = "0.2.2"
	jobWindowCheck  = "window-check"
	windowCheckCron = "* * * * *"
	buzzerAction    = "buzzer"
	displayCap      = "cloudpath.dev/capability/display-text@1"
	buzzerCap       = "cloudpath.dev/capability/buzzer@1"
	keyCap          = "cloudpath.dev/capability/key@1"
)

// window state values stored in domain records.
const (
	windowOpened    = "opened"
	windowCompleted = "completed"
	windowMissed    = "missed"
)

// keyPressEvent is the event type delivered by the key@1 capability. A key
// press on a bound compartment entity means "the user confirmed this
// compartment": that interpretation lives here, in the application, and never
// in a Driver.
const keyPressEvent = keyCap + "/press"

// windowTrack is the in-memory runtime state for one scheduled window instance.
type windowTrack struct {
	ID             string
	Compartment    string
	Start          time.Time
	End            time.Time
	State          string
	OpenedAt       time.Time
	ClosedAt       time.Time
	ReminderEntity string
	// 提醒命令的最终回执（RequestCompleted）。空 = 尚无最终回执；
	// 状态机不受它影响（只有按键完成窗口、窗口结束判 missed），
	// 但它让 missed 可区分「用户未响应」与「提醒从未送达设备」。
	ReminderState  string
	ReminderResult string
	ReminderDoneAt time.Time
}

// instanceState is the per-plugin-instance runtime state.
type instanceState struct {
	config    *Config
	configRev uint32
	bindings  map[string][]string
	windows   map[string]*windowTrack
	lastSeq   uint64
	jobs      map[string]string // idempotency key -> result JSON
}

// Service implements the ApplicationService protocol for the Scheduled
// Compartment reference application. It is device-agnostic: it only ever works
// with the entity ids supplied through capability bindings and never refers to
// a Driver id, port or vendor field.
type Service struct {
	pluginID  string
	version   string
	runtimeID string

	mu          sync.Mutex
	initialized bool
	closed      bool
	// writers 按实例路由 effect：共享进程里每个 App 实例有独立的 RPC 会话
	// （AppHost 的 appruntime 每实例一条 HandleEvents 流），effect 必须回到
	// 发起事件的那条流，否则对端按「实例不匹配」拒绝（2026-09-05 真板实测：
	// 两个实例同进程，后开的流覆盖全局 writer，先开实例的 effect 全被拒）。
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

// New returns a fresh, uninitialized Scheduled Compartment service.
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
		s.runtimeID = fmt.Sprintf("scheduled-compartment-%d", time.Now().UnixNano())
	}
	runtimeID := s.runtimeID
	s.mu.Unlock()

	return &application.InitializeResponse{
		NegotiatedProtocolVersion: negotiated,
		Status:                    status.New(),
		RuntimeID:                 runtimeID,
	}, nil
}

// Describe returns the stable application descriptor. It is the machine source
// of truth for requirements and jobs and must mirror plugin.yaml.
func (s *Service) Describe(context.Context) (*application.ApplicationDescriptor, error) {
	return &application.ApplicationDescriptor{
		ApplicationID:  s.pluginID,
		Version:        s.version,
		SchemaVersions: []string{application.SchemaVersion},
		Requirements: []application.RequirementDescriptor{
			{ID: "reminder-output", Capability: buzzerCap, Cardinality: "one"},
			{ID: "compartments", Capability: keyCap, Cardinality: "one-or-more", MinItems: 3},
			{ID: "local-display", Capability: displayCap, Cardinality: "zero-or-one"},
		},
		Jobs: []application.JobDescriptor{
			{ID: jobWindowCheck, Title: "Check for missed windows", InputSchemaJSON: `{"type":"object","properties":{"window_id":{"type":"string"}}}`},
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
		s.instance(req.PluginInstanceID).bindings = groupBindings(req.Bindings)
		s.mu.Unlock()
	}
	return &application.ValidateBindingResponse{Valid: valid, Issues: issues}, nil
}

// ---------------------------------------------------------------------------
// Event stream
// ---------------------------------------------------------------------------

// HandleEvents is the bidi event/effect stream. Core sends events here and the
// plugin emits Core-approved effects back over the same stream.
//
// 共享进程多实例：appruntime 为每个实例开独立的 HandleEvents 会话，事件
// 到达时把该实例登记到本流，effect 由此路由回正确的流。
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
	case *application.ScheduleTick:
		return s.onScheduleTick(instanceID, u)
	case *application.CapabilityEvent:
		return s.onCapabilityEvent(instanceID, u)
	case *application.RequestCompleted:
		return s.onRequestCompleted(instanceID, u)
	case *application.InstanceLifecycle:
		return nil
	default:
		return nil
	}
}

func (s *Service) onScheduleTick(instanceID string, tick *application.ScheduleTick) error {
	if tick == nil {
		return nil
	}
	w, err := parseWindowTick(tick.WindowJSON)
	if err != nil {
		return nil // malformed schedule tick is ignored, never crashes the stream
	}

	s.mu.Lock()
	st := s.instance(instanceID)
	if st.config == nil {
		s.mu.Unlock()
		return nil // not configured yet
	}
	if _, exists := st.windows[w.ID]; exists {
		s.mu.Unlock()
		return nil // already tracked; idempotent
	}
	if !st.config.hasCompartment(w.Compartment) {
		s.mu.Unlock()
		return nil // unknown compartment
	}
	w.ReminderEntity = reminderEntity(st)
	st.windows[w.ID] = w
	effects := s.windowStartEffects(st, w)
	s.mu.Unlock()

	return s.flush(instanceID, effects)
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

// onKeyEvent interprets a key press on a bound compartment entity as the user
// confirming that compartment. The business meaning of the generic key@1
// event lives entirely here.
func (s *Service) onKeyEvent(instanceID string, ev *application.CapabilityEvent) error {
	s.mu.Lock()
	st := s.instance(instanceID)
	compID := keyToCompartment(st, ev.EntityID)
	if compID == "" {
		s.mu.Unlock()
		return nil // not a bound compartment entity
	}
	var active *windowTrack
	for _, w := range st.windows {
		if w.Compartment == compID && w.State == windowOpened {
			active = w
			break
		}
	}
	if active == nil {
		s.mu.Unlock()
		return nil // no active window for this compartment; idempotent
	}
	active.State = windowCompleted
	active.ClosedAt = parseOccurred(ev.OccurredAt)
	effects := []application.ApplicationEffectUnion{
		windowRecord(active),
		cancelTaskEffect(active.ID),
	}
	s.mu.Unlock()

	return s.flush(instanceID, effects)
}

// reminderRequestPrefix 是提醒命令的幂等键前缀（windowStartEffects 生成，
// AppHost 原样回传为 RequestCompleted.RequestID），用于把回执关联回窗口。
const reminderRequestPrefix = "reminder-"

func (s *Service) onRequestCompleted(instanceID string, ev *application.RequestCompleted) error {
	// RequestCompleted 关闭提醒命令生命周期。状态机不受影响：只有按键
	// 完成窗口、window-check job 判 missed——但命令结局必须持久化到窗口
	// 记录，否则 missed 无法区分「用户未响应」与「提醒从未送达设备」。
	if ev == nil || !strings.HasPrefix(ev.RequestID, reminderRequestPrefix) {
		return nil
	}
	var reminderState string
	switch ev.State {
	case application.CommandStateSucceeded:
		reminderState = "succeeded"
	case application.CommandStateFailed:
		reminderState = "failed"
	case application.CommandStateTimedOut:
		reminderState = "timedout"
	default:
		return nil // 中间态不出现在 RequestCompleted；窗口记录保持不变
	}
	windowID := strings.TrimPrefix(ev.RequestID, reminderRequestPrefix)

	s.mu.Lock()
	st := s.instance(instanceID)
	w := st.windows[windowID]
	if w == nil {
		s.mu.Unlock()
		return nil // 未知窗口（已重启丢失或伪 RequestID）：幂等忽略
	}
	w.ReminderState = reminderState
	w.ReminderResult = ev.ResultJSON
	w.ReminderDoneAt = s.now()
	effects := []application.ApplicationEffectUnion{windowRecord(w)}
	s.mu.Unlock()

	return s.flush(instanceID, effects)
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
		return nil // no active stream for this instance; nothing to emit
	}
	eff := &application.ApplicationEffect{
		PluginInstanceID: instanceID,
		Sequence:         seq,
		SchemaVersion:    application.SchemaVersion,
		Union:            union,
	}
	return writer.Send(context.Background(), eff)
}

// ---------------------------------------------------------------------------
// HTTP subroute / jobs / health / shutdown
// ---------------------------------------------------------------------------

// HandleRequest serves the plugin-scoped HTTP subroute. It is read-only and
// returns a bounded JSON summary of the instance config and window state.
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
			statusBody["timezone"] = st.config.Timezone
			statusBody["compartments"] = len(st.config.Compartments)
			statusBody["windows"] = len(st.windows)
			statusBody["active"] = activeCount(st)
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

// RunJob executes the window-check job. It scans active windows against the
// (injectable) clock and emits a missed domain record for any that have
// expired without completing. RunJob is idempotent per IdempotencyKey.
func (s *Service) RunJob(_ context.Context, req *application.RunJobRequest) (*application.RunJobResponse, error) {
	if req == nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "nil job request")
	}
	if req.JobID != jobWindowCheck {
		return nil, status.Errorf(status.CodeUnimplemented, "job %q is not implemented", req.JobID)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, status.Errorf(status.CodeUnavailable, "plugin is shutting down")
	}
	st := s.instance(req.PluginInstanceID)
	if st.config == nil {
		s.mu.Unlock()
		return &application.RunJobResponse{JobID: req.JobID, Status: status.New(), ResultJSON: resultJSON(nil)}, nil
	}
	if req.IdempotencyKey != "" {
		if prev, ok := st.jobs[req.IdempotencyKey]; ok {
			s.mu.Unlock()
			return &application.RunJobResponse{JobID: req.JobID, Status: status.New(), ResultJSON: prev}, nil
		}
	}

	now := s.now()
	var missed []string
	var effects []application.ApplicationEffectUnion
	for id, w := range st.windows {
		if w.State == windowOpened && !now.Before(w.End) {
			w.State = windowMissed
			missed = append(missed, id)
			effects = append(effects, windowRecord(w), cancelTaskEffect(w.ID), missedNotificationEffect(w))
		}
	}
	sort.Strings(missed)
	body := resultJSON(missed)
	if req.IdempotencyKey != "" {
		st.jobs[req.IdempotencyKey] = body
	}
	s.mu.Unlock()

	for _, u := range effects {
		if err := s.sendEffect(req.PluginInstanceID, u); err != nil {
			return nil, err
		}
	}
	return &application.RunJobResponse{JobID: req.JobID, Status: status.New(), ResultJSON: body}, nil
}

// Health reports serving state per configured instance.
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

// Shutdown marks the service as closed. Subsequent RPCs fail grace-fully.
func (s *Service) Shutdown(_ context.Context, _ *application.ShutdownRequest) (*application.ShutdownResponse, error) {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return &application.ShutdownResponse{Status: status.New()}, nil
}

// ---------------------------------------------------------------------------
// helper functions
// ---------------------------------------------------------------------------

func (s *Service) instance(id string) *instanceState {
	if s.instances == nil {
		s.instances = map[string]*instanceState{}
	}
	st, ok := s.instances[id]
	if !ok {
		st = &instanceState{
			bindings: map[string][]string{},
			windows:  map[string]*windowTrack{},
			jobs:     map[string]string{},
		}
		s.instances[id] = st
	}
	return st
}

func (s *Service) windowStartEffects(st *instanceState, w *windowTrack) []application.ApplicationEffectUnion {
	effects := []application.ApplicationEffectUnion{windowRecord(w)}
	if w.ReminderEntity != "" {
		policy := Reminder{Freq: 1, Duration: 1}
		if st != nil && st.config != nil {
			policy = st.config.ResolvedReminder()
		}
		effects = append(effects, &application.RequestCommand{
			EntityID:       w.ReminderEntity,
			Action:         buzzerAction,
			ArgsJSON:       mustJSON(map[string]int{"freq": policy.Freq, "duration": policy.Duration}),
			IdempotencyKey: "reminder-" + w.ID,
			Deadline:       w.End.UTC().Format(time.RFC3339),
		})
	}
	effects = append(effects, &application.ScheduleTask{
		ScheduleID:  "window-check-" + w.ID,
		Cron:        windowCheckCron,
		PayloadJSON: mustJSON(map[string]any{"window_id": w.ID}),
	})
	return effects
}

// validateBindings enforces the declared requirement cardinalities and rejects
// any requirement id that is not part of this application (which structurally
// rules out Driver coupling).
func validateBindings(bindings []application.Binding) []application.BindingIssue {
	allowed := map[string]struct{}{"reminder-output": {}, "compartments": {}, "local-display": {}}
	counts := map[string]int{}
	entityOK := map[string]bool{}

	var issues []application.BindingIssue

	for _, b := range bindings {
		if _, ok := allowed[b.RequirementID]; !ok {
			issues = append(issues, application.BindingIssue{
				RequirementID: b.RequirementID,
				Severity:      "error",
				Message:       fmt.Sprintf("requirement %q is not declared by this application", b.RequirementID),
			})
			continue
		}
		counts[b.RequirementID]++
		if strings.TrimSpace(b.EntityID) == "" {
			issues = append(issues, application.BindingIssue{
				RequirementID: b.RequirementID,
				Severity:      "error",
				Message:       "entity_id must not be empty",
			})
		}
		if b.RequirementID == "compartments" {
			key := b.RequirementID + "\x00" + b.EntityID
			if entityOK[key] {
				issues = append(issues, application.BindingIssue{
					RequirementID: b.RequirementID,
					Severity:      "error",
					Message:       fmt.Sprintf("duplicate compartment entity %q", b.EntityID),
				})
			}
			entityOK[key] = true

		}
	}

	if counts["reminder-output"] != 1 {
		issues = append(issues, application.BindingIssue{
			RequirementID: "reminder-output",
			Severity:      "error",
			Message:       fmt.Sprintf("reminder-output requires exactly one binding, got %d", counts["reminder-output"]),
		})
	}
	if counts["compartments"] < 3 {
		issues = append(issues, application.BindingIssue{
			RequirementID: "compartments",
			Severity:      "error",
			Message:       fmt.Sprintf("compartments requires at least 3 bindings, got %d", counts["compartments"]),
		})
	}
	if counts["local-display"] > 1 {
		issues = append(issues, application.BindingIssue{
			RequirementID: "local-display",
			Severity:      "error",
			Message:       "local-display allows at most one binding",
		})
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

func (c *Config) hasCompartment(id string) bool {
	for _, cp := range c.Compartments {
		if cp.ID == id {
			return true
		}
	}
	return false
}

func reminderEntity(st *instanceState) string {
	if st == nil {
		return ""
	}
	if ids := st.bindings["reminder-output"]; len(ids) > 0 {
		return ids[0]
	}
	return ""
}

func keyToCompartment(st *instanceState, entity string) string {
	if st == nil || st.config == nil {
		return ""
	}
	keys := st.bindings["compartments"]
	comps := st.config.Compartments
	for i, c := range keys {
		if i >= len(comps) {
			break
		}
		if c == entity {
			return comps[i].ID
		}
	}
	return ""
}

// keyToCompartment maps a bound key entity to its configured compartment by
// binding order. The application gives business meaning to the key: the
// Driver only reports a generic key press.

func activeCount(st *instanceState) int {
	n := 0
	for _, w := range st.windows {
		if w.State == windowOpened {
			n++
		}
	}
	return n
}

// parseWindowTick parses the concrete schedule window delivered in
// ScheduleTick.WindowJSON. Runtime windows use RFC3339 timestamps so the app
// can compute a deterministic deadline.
func parseWindowTick(raw string) (*windowTrack, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("window_json is empty")
	}
	var t struct {
		ID          string `json:"id"`
		Compartment string `json:"compartment"`
		Start       string `json:"start"`
		End         string `json:"end"`
	}
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(t.ID)
	if id == "" {
		return nil, fmt.Errorf("window id is required")
	}
	comp := strings.TrimSpace(t.Compartment)
	if comp == "" {
		return nil, fmt.Errorf("window compartment is required")
	}
	start, err := time.Parse(time.RFC3339, strings.TrimSpace(t.Start))
	if err != nil {
		return nil, fmt.Errorf("window start %q is not RFC3339", t.Start)
	}
	end, err := time.Parse(time.RFC3339, strings.TrimSpace(t.End))
	if err != nil {
		return nil, fmt.Errorf("window end %q is not RFC3339", t.End)
	}
	if !end.After(start) {
		return nil, fmt.Errorf("window end must be after start")
	}
	return &windowTrack{ID: id, Compartment: comp, Start: start, End: end, State: windowOpened}, nil
}

func parseOccurred(s string) time.Time {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}
	}
	return t
}

func windowRecord(w *windowTrack) *application.UpsertDomainRecord {
	data := map[string]any{
		"id":               w.ID,
		"compartment":      w.Compartment,
		"start":            w.Start.UTC().Format(time.RFC3339),
		"end":              w.End.UTC().Format(time.RFC3339),
		"state":            w.State,
		"opened_at":        optTime(w.OpenedAt),
		"closed_at":        optTime(w.ClosedAt),
		"reminder_entity":  w.ReminderEntity,
		"reminder_state":   w.ReminderState,
		"reminder_result":  w.ReminderResult,
		"reminder_done_at": optTime(w.ReminderDoneAt),
	}
	return &application.UpsertDomainRecord{
		RecordType: "window",
		RecordID:   w.ID,
		DataJSON:   mustJSON(data),
		Version:    "1",
	}
}

func cancelTaskEffect(windowID string) *application.CancelScheduledTask {
	return &application.CancelScheduledTask{ScheduleID: "window-check-" + windowID}
}

func missedNotificationEffect(w *windowTrack) *application.SendNotification {
	return &application.SendNotification{
		Title:    "Scheduled window missed",
		Body:     fmt.Sprintf("Window %s for compartment %s was not completed in time", w.ID, w.Compartment),
		Severity: "warning",
	}
}

func resultJSON(ids []string) string {
	if ids == nil {
		ids = []string{}
	}
	return mustJSON(map[string]any{"missed": ids})
}

func optTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
