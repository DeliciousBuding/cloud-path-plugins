// SPDX-License-Identifier: Apache-2.0

package buttonindicator

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/status"
)

const (
	jobRequest            = "request"
	jobAcknowledge        = "acknowledge"
	jobAcknowledgePending = "acknowledge-pending"
	callRecordType        = "service_call"
	callDebounce          = 250 * time.Millisecond
	callResultWait        = 10 * time.Second
	pendingLEDMask        = 255

	// Required explicit input also fails closed on hosts that ignore ManualOnly.
	requestJobSchema = `{"type":"object","properties":{"confirm":{"type":"boolean","const":true,"title":"确认发起呼叫"},"note":{"type":"string","maxLength":256,"title":"请求说明（可选）"}},"required":["confirm"],"additionalProperties":false}`
	// Keep the console text form: pattern is unsupported; RunJob validates whitespace.
	acknowledgeJobSchema = `{"type":"object","properties":{"request_id":{"type":"string","minLength":1,"maxLength":128,"title":"待处理请求编号","description":"使用发起呼叫返回的 request_id；不会确认其它请求。"}},"required":["request_id"],"additionalProperties":false}`
	// 零参数确认：直接解除当前待处理呼叫。普通用户不该、也无法手抄 request_id。
	acknowledgePendingJobSchema = `{"type":"object","properties":{},"additionalProperties":false}`
)

// A business acknowledgement and an actuator result are different facts.
// Records are persisted by Core via UpsertDomainRecord, but the public SDK has
// no read/restore primitive. Everything in callRuntime is current-process only.
type callRecord struct {
	RequestID         string         `json:"request_id"`
	Status            string         `json:"status"`
	Source            string         `json:"source"`
	RequestEntity     string         `json:"request_entity,omitempty"`
	Note              string         `json:"note,omitempty"`
	RequestedAt       string         `json:"requested_at"`
	AcknowledgedAt    string         `json:"acknowledged_at,omitempty"`
	AcknowledgedBy    string         `json:"acknowledged_by,omitempty"`
	AcknowledgeEntity string         `json:"acknowledge_entity,omitempty"`
	IndicatorOn       *commandResult `json:"indicator_on"`
	IndicatorOff      *commandResult `json:"indicator_off,omitempty"`
	Sound             *commandResult `json:"sound,omitempty"`
	Revision          uint64         `json:"revision"`
	dirty             bool
}

// State is awaiting_result until Core reports a terminal RequestCompleted.
// result_timeout means only that this process did not observe a result within
// the deadline; it does NOT assert that the device failed or the LED is off.
type commandResult struct {
	CommandID       string `json:"command_id"`
	EntityID        string `json:"entity_id"`
	Action          string `json:"action"`
	DesiredMask     *int   `json:"desired_mask,omitempty"`
	State           string `json:"state"`
	Deadline        string `json:"deadline"`
	ObservedAt      string `json:"observed_at,omitempty"`
	ResultTimeoutAt string `json:"result_timeout_at,omitempty"`
	ResultJSON      string `json:"result_json,omitempty"`
	ErrorCode       string `json:"error_code,omitempty"`
}

type callCommand struct {
	record   *callRecord
	result   *commandResult
	effect   *application.RequestCommand
	deadline time.Time
	timer    *time.Timer
}

type callJob struct {
	args      string
	requestID string
}

type callRuntime struct {
	pendingID       string
	lastID          string
	lastIndicatorID string
	records         map[string]*callRecord
	commands        map[string]*callCommand
	jobs            map[string]callJob
	lastPress       map[string]time.Time
}

// All call fields use Service.mu; callMu additionally serializes transitions
// through effect emission so an old off cannot be emitted after a new on.
func (s *Service) lockCallInstance(instanceID string) *instanceState {
	s.mu.Lock()
	st := s.instance(instanceID)
	s.mu.Unlock()
	st.callMu.Lock()
	s.mu.Lock()
	return st
}

func (st *instanceState) callState() *callRuntime {
	if st.calls == nil {
		st.calls = &callRuntime{
			records: map[string]*callRecord{}, commands: map[string]*callCommand{},
			jobs: map[string]callJob{}, lastPress: map[string]time.Time{},
		}
	}
	return st.calls
}

func (s *Service) callReady(instanceID string, st *instanceState) error {
	if s.closed || s.writers[instanceID] == nil {
		return status.Errorf(status.CodeUnavailable, "service-call requires an active effect stream")
	}
	if st.config == nil || st.config.ResolvedMode() != modeServiceCall {
		return status.Errorf(status.CodeFailedPrecondition, "set mode to service-call (or call) before running this job")
	}
	if len(st.bindings["button-input"]) == 0 || firstEntity(st.bindings, "indicator") == "" {
		return status.Errorf(status.CodeFailedPrecondition, "bind button-input and indicator before requesting service")
	}
	return nil
}

func decodeCallArgs(raw string, dst any, allowed ...string) error {
	text := strings.TrimSpace(raw)
	if !strings.HasPrefix(text, "{") {
		return status.Errorf(status.CodeInvalidArgument, "args_json must be an explicit JSON object")
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return status.Errorf(status.CodeInvalidArgument, "invalid job arguments: %v", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return status.Errorf(status.CodeInvalidArgument, "args_json must contain exactly one object")
	}
	// encoding/json otherwise accepts null for string fields, unlike the schema.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &fields); err != nil {
		return status.Errorf(status.CodeInvalidArgument, "invalid job arguments: %v", err)
	}
	for name, value := range fields {
		// The JSON schema is case-sensitive; encoding/json struct matching is
		// not, even with DisallowUnknownFields. Enforce exact public names.
		known := false
		for _, key := range allowed {
			known = known || name == key
		}
		if !known {
			return status.Errorf(status.CodeInvalidArgument, "unknown argument %q", name)
		}
		if strings.TrimSpace(string(value)) == "null" {
			return status.Errorf(status.CodeInvalidArgument, "argument %q cannot be null", name)
		}
	}
	return nil
}

func (s *Service) runCallJob(req *application.RunJobRequest) (*application.RunJobResponse, error) {
	if req.JobType == "scheduled" {
		return nil, status.Errorf(status.CodeFailedPrecondition, "service-call actions are manual-only")
	}
	var note, requestID, canonicalArgs string
	switch req.JobID {
	case jobRequest:
		var args struct {
			Confirm bool   `json:"confirm"`
			Note    string `json:"note"`
		}
		if err := decodeCallArgs(req.ArgsJSON, &args, "confirm", "note"); err != nil {
			return nil, err
		}
		if !args.Confirm || utf8.RuneCountInString(args.Note) > 256 {
			return nil, status.Errorf(status.CodeInvalidArgument, "request requires confirm=true and a note of at most 256 characters")
		}
		note, canonicalArgs = args.Note, mustJSON(args)
	case jobAcknowledge:
		var args struct {
			RequestID string `json:"request_id"`
		}
		if err := decodeCallArgs(req.ArgsJSON, &args, "request_id"); err != nil {
			return nil, err
		}
		if args.RequestID == "" || utf8.RuneCountInString(args.RequestID) > 128 || strings.IndexFunc(args.RequestID, unicode.IsSpace) >= 0 {
			return nil, status.Errorf(status.CodeInvalidArgument, "acknowledge requires the exact non-empty request_id (no whitespace)")
		}
		requestID, canonicalArgs = args.RequestID, mustJSON(args)
	case jobAcknowledgePending:
		var args struct{}
		if err := decodeCallArgs(req.ArgsJSON, &args); err != nil {
			return nil, err
		}
		canonicalArgs = "{}"
	}

	st := s.lockCallInstance(req.PluginInstanceID)
	defer st.callMu.Unlock()
	if err := s.callReady(req.PluginInstanceID, st); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	calls := st.callState()
	jobKey := req.JobID + "\x00" + req.IdempotencyKey
	if req.IdempotencyKey != "" {
		if previous, ok := calls.jobs[jobKey]; ok {
			if previous.args != canonicalArgs {
				s.mu.Unlock()
				return nil, status.Errorf(status.CodeAlreadyExists, "idempotency_key already used with different arguments")
			}
			record := calls.records[previous.requestID]
			s.mu.Unlock()
			if err := s.emitCallTransition(req.PluginInstanceID, record, nil); err != nil {
				return nil, err
			}
			return s.callJobResponse(req.JobID, record, false), nil
		}
	}

	if req.JobID == jobAcknowledgePending {
		requestID = calls.pendingID
		if requestID == "" {
			s.mu.Unlock()
			return nil, status.Errorf(status.CodeFailedPrecondition, "there is no pending call to clear")
		}
	}

	var record *callRecord
	var commands []*callCommand
	var created bool
	var err error
	if req.JobID == jobRequest {
		record, commands, created = s.requestCall(st, "job", "", note)
	} else {
		record, commands, err = s.acknowledgeCall(st, requestID, "job", "")
	}
	// Remember the identity even if emission later fails: an uncertain retry
	// must never create a different call after the first one is acknowledged.
	if err == nil && req.IdempotencyKey != "" {
		calls.jobs[jobKey] = callJob{args: canonicalArgs, requestID: record.RequestID}
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := s.emitCallTransition(req.PluginInstanceID, record, commands); err != nil {
		return nil, err
	}
	return s.callJobResponse(req.JobID, record, created), nil
}

func (s *Service) callJobResponse(jobID string, record *callRecord, created bool) *application.RunJobResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &application.RunJobResponse{
		JobID: jobID, Status: status.New(),
		ResultJSON: mustJSON(map[string]any{
			"request_id": record.RequestID, "status": record.Status,
			"created": created, "request": record,
		}),
	}
}

func (s *Service) onServiceCallKey(instanceID string, ev *application.CapabilityEvent) error {
	// Bind a queued physical acknowledgement to the request visible at event
	// receipt, before waiting behind another transition's effect sends.
	s.mu.Lock()
	observedID := ""
	if calls := s.instance(instanceID).calls; calls != nil {
		observedID = calls.pendingID
	}
	s.mu.Unlock()
	st := s.lockCallInstance(instanceID)
	defer st.callMu.Unlock()
	if st.config == nil || st.config.ResolvedMode() != modeServiceCall || s.closed {
		s.mu.Unlock()
		return nil
	}
	request := ev.RequirementID == "button-input" && st.bound("button-input", ev.EntityID)
	ack := ev.RequirementID == "acknowledge-input" && st.bound("acknowledge-input", ev.EntityID)
	if !request && !ack {
		s.mu.Unlock()
		return nil
	}
	calls := st.callState()
	at := s.now() // retain the monotonic clock for debounce
	last := calls.lastPress[ev.EntityID]
	calls.lastPress[ev.EntityID] = at
	if !last.IsZero() && at.Sub(last) < callDebounce {
		s.mu.Unlock()
		return nil
	}
	if ack && (observedID == "" || calls.pendingID != observedID) {
		s.mu.Unlock()
		return nil
	}
	if err := s.callReady(instanceID, st); err != nil {
		s.mu.Unlock()
		return err
	}
	var record *callRecord
	var commands []*callCommand
	if request {
		record, commands, _ = s.requestCall(st, "button", ev.EntityID, "")
	} else {
		record = calls.records[calls.pendingID]
		// Reject an acknowledgement known to predate this call. Core sources
		// can have second precision; the key event itself carries no call ID.
		if ev.OccurredAt != "" {
			occurred, err := time.Parse(time.RFC3339Nano, ev.OccurredAt)
			requested, _ := time.Parse(time.RFC3339Nano, record.RequestedAt)
			if err != nil || occurred.Before(requested.Truncate(time.Second)) {
				s.mu.Unlock()
				return nil
			}
		}
		// Snapshot the exact current ID while the transition lock is held.
		record, commands, _ = s.acknowledgeCall(st, record.RequestID, "button", ev.EntityID)
	}
	s.mu.Unlock()
	return s.emitCallTransition(instanceID, record, commands)
}

// The following transition builders require both Service.mu and callMu.
func (s *Service) requestCall(st *instanceState, source, entity, note string) (*callRecord, []*callCommand, bool) {
	calls := st.callState()
	if calls.pendingID != "" {
		return calls.records[calls.pendingID], nil, false
	}
	record := &callRecord{
		RequestID: "call-" + strings.ToLower(rand.Text()), Status: "pending",
		Source: source, RequestEntity: entity, Note: note,
		RequestedAt: s.now().UTC().Format(time.RFC3339Nano), Revision: 1, dirty: true,
	}
	calls.records[record.RequestID] = record
	calls.pendingID, calls.lastID = record.RequestID, record.RequestID
	on := s.newCallCommand(st, record, "on", firstEntity(st.bindings, "indicator"), ledAction, pendingLEDMask)
	record.IndicatorOn = on.result
	commands := []*callCommand{on}
	if sound := firstEntity(st.bindings, "sound"); st.config.BeepOnPress && sound != "" {
		beep := s.newCallCommand(st, record, "sound", sound, buzzerAct, 0)
		record.Sound = beep.result
		commands = append(commands, beep)
	}
	return record, commands, true
}

func (s *Service) acknowledgeCall(st *instanceState, requestID, source, entity string) (*callRecord, []*callCommand, error) {
	calls := st.callState()
	record := calls.records[requestID]
	if record == nil {
		return nil, nil, status.Errorf(status.CodeNotFound, "request %q is not known in this runtime; persisted records are not restored by this SDK", requestID)
	}
	if record.Status == "acknowledged" {
		return record, nil, nil // never turn off a newer request on an old retry
	}
	if calls.pendingID != requestID {
		return nil, nil, status.Errorf(status.CodeFailedPrecondition, "request_id does not match the pending call")
	}
	record.Status = "acknowledged"
	record.AcknowledgedAt = s.now().UTC().Format(time.RFC3339Nano)
	record.AcknowledgedBy, record.AcknowledgeEntity = source, entity
	record.Revision++
	record.dirty = true
	calls.pendingID = ""
	off := s.newCallCommand(st, record, "off", record.IndicatorOn.EntityID, ledAction, 0)
	record.IndicatorOff = off.result
	return record, []*callCommand{off}, nil
}

func (s *Service) newCallCommand(st *instanceState, record *callRecord, phase, entity, action string, mask int) *callCommand {
	deadline := s.now().Add(callResultWait) // retain monotonic time internally
	key := record.RequestID + ":" + phase
	result := &commandResult{CommandID: key, EntityID: entity, Action: action, State: "awaiting_result", Deadline: deadline.UTC().Format(time.RFC3339Nano)}
	args := mustJSON(map[string]int{"freq": 1, "duration": 1})
	if action == ledAction {
		result.DesiredMask = &mask
		args = mustJSON(map[string]int{"mask": mask})
		st.calls.lastIndicatorID = key
	}
	cmd := &callCommand{
		record: record, result: result, deadline: deadline,
		effect: &application.RequestCommand{EntityID: entity, Action: action, ArgsJSON: args, IdempotencyKey: key, Deadline: result.Deadline},
	}
	st.calls.commands[key] = cmd
	return cmd
}

func (s *Service) persistCallRecord(instanceID string, record *callRecord) error {
	s.mu.Lock()
	if !record.dirty {
		s.mu.Unlock()
		return nil
	}
	effect := &application.UpsertDomainRecord{
		RecordType: callRecordType, RecordID: record.RequestID,
		DataJSON: mustJSON(record), Version: fmt.Sprint(record.Revision),
	}
	s.mu.Unlock()
	if err := s.emitEffect(instanceID, effect, true); err != nil {
		return err
	}
	s.mu.Lock()
	record.dirty = false
	s.mu.Unlock()
	return nil
}

// callMu remains held through the record and command sends. A send is only an
// intent delivered to Core, not device success (nor an acknowledgement of the
// domain-store write). Errors remain in memory and are re-upserted on reconnect.
func (s *Service) emitCallTransition(instanceID string, record *callRecord, commands []*callCommand) error {
	if err := s.persistCallRecord(instanceID, record); err != nil {
		s.markCallDeliveryError(record, commands, false, err)
		return err
	}
	for i, cmd := range commands {
		if err := s.emitEffect(instanceID, cmd.effect, true); err != nil {
			s.markCallDeliveryError(record, commands[i:], true, err)
			_ = s.persistCallRecord(instanceID, record) // best effort; dirty survives failure
			return err
		}
		s.mu.Lock()
		if !s.closed {
			id := cmd.result.CommandID
			cmd.timer = time.AfterFunc(max(0, cmd.deadline.Sub(s.now())), func() { s.expireCallCommand(instanceID, id) })
		}
		s.mu.Unlock()
	}
	return nil
}

func (s *Service) markCallDeliveryError(record *callRecord, commands []*callCommand, attempted bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, cmd := range commands {
		cmd.result.State = "not_dispatched"
		if i == 0 && attempted {
			cmd.result.State = "delivery_unknown" // failed Send can be ambiguous
		}
		cmd.result.ErrorCode = "EFFECT_DELIVERY_FAILED"
		cmd.result.ResultJSON = mustJSON(map[string]string{"error": err.Error()})
		cmd.result.ObservedAt = s.now().UTC().Format(time.RFC3339Nano)
	}
	record.Revision++
	record.dirty = true
}

func (s *Service) onCallCommandCompleted(instanceID string, ev *application.RequestCompleted) error {
	if ev == nil {
		return nil
	}
	var state string
	switch ev.State {
	case application.CommandStateSucceeded:
		state = "succeeded"
	case application.CommandStateFailed:
		state = "failed"
	case application.CommandStateTimedOut:
		state = "timed_out"
	case application.CommandStateCancelled:
		state = "cancelled"
	default:
		return nil // sent/accepted/running is NOT completion
	}
	st := s.lockCallInstance(instanceID)
	defer st.callMu.Unlock()
	if st.calls == nil || s.closed {
		s.mu.Unlock()
		return nil
	}
	cmd := st.calls.commands[ev.RequestID]
	if cmd == nil || cmd.result.EntityID != ev.EntityID || cmd.result.Action != ev.Action ||
		(cmd.result.State != "awaiting_result" && cmd.result.State != "result_timeout" && cmd.result.State != "delivery_unknown") {
		s.mu.Unlock()
		return nil // mismatched, duplicate or stale terminal result
	}
	if cmd.timer != nil {
		cmd.timer.Stop()
		cmd.timer = nil
	}
	cmd.result.State, cmd.result.ErrorCode, cmd.result.ResultJSON = state, ev.ErrorCode, ev.ResultJSON
	cmd.result.ObservedAt = s.now().UTC().Format(time.RFC3339Nano)
	cmd.record.Revision++
	cmd.record.dirty = true
	s.mu.Unlock()
	return s.persistCallRecord(instanceID, cmd.record)
}

func (s *Service) expireCallCommand(instanceID, commandID string) {
	st := s.lockCallInstance(instanceID)
	defer st.callMu.Unlock()
	if st.calls == nil || s.closed {
		s.mu.Unlock()
		return
	}
	cmd := st.calls.commands[commandID]
	if cmd == nil || cmd.result.State != "awaiting_result" || s.now().Before(cmd.deadline) {
		s.mu.Unlock()
		return
	}
	cmd.timer = nil
	cmd.result.State = "result_timeout"
	cmd.result.ErrorCode = "RESULT_NOT_OBSERVED"
	cmd.result.ResultTimeoutAt = s.now().UTC().Format(time.RFC3339Nano)
	cmd.result.ObservedAt = cmd.result.ResultTimeoutAt
	cmd.record.Revision++
	cmd.record.dirty = true
	s.mu.Unlock()
	_ = s.persistCallRecord(instanceID, cmd.record)
}

func (s *Service) flushCallRecords(instanceID string) error {
	st := s.lockCallInstance(instanceID)
	defer st.callMu.Unlock()
	var records []*callRecord
	if st.calls != nil {
		for _, record := range st.calls.records {
			if record.dirty {
				records = append(records, record)
			}
		}
	}
	s.mu.Unlock()
	for _, record := range records {
		if err := s.persistCallRecord(instanceID, record); err != nil {
			return err
		}
	}
	return nil
}

func (st *instanceState) callBusy() bool {
	if st.calls == nil {
		return false
	}
	if st.calls.pendingID != "" {
		return true
	}
	for _, cmd := range st.calls.commands {
		if cmd.result.State == "awaiting_result" {
			return true
		}
	}
	return false
}

func sameBindings(current map[string][]string, proposed []application.Binding) bool {
	next := groupBindings(proposed)
	if len(current) != len(next) {
		return false
	}
	for requirement, entities := range current {
		if len(entities) != len(next[requirement]) {
			return false
		}
		for _, entity := range entities {
			found := false
			for _, candidate := range next[requirement] {
				found = found || candidate == entity
			}
			if !found {
				return false
			}
		}
	}
	return true
}

func (st *instanceState) addCallSummary(body map[string]any) {
	body["runtime_scope"] = "current_process_only"
	body["pending_request_id"] = ""
	body["request_count"] = 0
	body["request"] = nil
	body["indicator"] = nil
	if st.calls == nil {
		return
	}
	body["pending_request_id"] = st.calls.pendingID
	body["request_count"] = len(st.calls.records)
	body["request"] = st.calls.records[st.calls.lastID]
	if cmd := st.calls.commands[st.calls.lastIndicatorID]; cmd != nil {
		body["indicator"] = cmd.result
	}
}

// Human-readable presentation stays with the business plugin. Core renders
// title/summary generically and does not maintain a domain-status dictionary.
func (r *callRecord) MarshalJSON() ([]byte, error) {
	type fields callRecord
	summary := "等待人工确认；指示灯："
	result := r.IndicatorOn
	if r.Status == "acknowledged" {
		summary = "已人工确认；解除指示灯："
		result = r.IndicatorOff
	}
	label := "尚未请求"
	if result != nil {
		switch result.State {
		case "succeeded":
			label = "设备已回执"
		case "awaiting_result":
			label = "等待设备回执"
		case "failed", "timed_out", "cancelled":
			label = "未成功，请检查设备"
		case "result_timeout", "delivery_unknown":
			label = "结果未知，请核对设备"
		default:
			label = "尚未完成"
		}
	}
	return json.Marshal(struct {
		*fields
		Title   string `json:"title"`
		Summary string `json:"summary"`
	}{(*fields)(r), "工位呼叫", summary + label})
}
