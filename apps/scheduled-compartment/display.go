package scheduledcompartment

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
)

const (
	maxDisplayArgsBytes   = 2048
	displayRequestPrefix  = "display-"
	displayAction         = "display"
	displayReminder       = "reminder"
	displayMissed         = "missed"
	displayIdle           = "idle"
	displayCommandTimeout = 30 * time.Second
)

// DisplayPolicy is an explicit, capability-owned protocol policy. The app
// validates bounded JSON objects, not vendor encodings, fonts or glyphs.
// No policy or no local-display binding means no display command.
type DisplayPolicy struct {
	ReminderArgs json.RawMessage `json:"reminder_args"`
	MissedArgs   json.RawMessage `json:"missed_args"`
	IdleArgs     json.RawMessage `json:"idle_args"`
}

func (p DisplayPolicy) Validate() error {
	for _, field := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"reminder_args", p.ReminderArgs}, {"missed_args", p.MissedArgs}, {"idle_args", p.IdleArgs},
	} {
		if _, err := canonicalDisplayArgs(field.raw); err != nil {
			return fmt.Errorf("display.%s: %w", field.name, err)
		}
	}
	return nil
}

func canonicalDisplayArgs(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || len(raw) > maxDisplayArgsBytes {
		return "", fmt.Errorf("must be a JSON object of at most %d bytes", maxDisplayArgsBytes)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var object map[string]any
	if err := dec.Decode(&object); err != nil {
		return "", fmt.Errorf("must be a JSON object: %w", err)
	}
	if len(object) == 0 {
		return "", fmt.Errorf("must be a non-empty JSON object")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return "", fmt.Errorf("must contain exactly one JSON object")
	}
	encoded, err := json.Marshal(object)
	if err != nil || len(encoded) > maxDisplayArgsBytes {
		return "", fmt.Errorf("encoded JSON object must be at most %d bytes", maxDisplayArgsBytes)
	}
	return string(encoded), nil
}

type displayTarget struct {
	State    string
	EntityID string
	ArgsJSON string
}

type displayCommand struct {
	RequestID   string
	Target      displayTarget
	State       string
	RequestedAt time.Time
	Deadline    time.Time
	DoneAt      time.Time
	ResultJSON  string
	ErrorCode   string
}

// One command may be pending per instance. Desired coalesces intervening state
// changes instead of queuing stale idle/reminder transitions. This is live
// process state only, not a new persistence or recovery mechanism.
type displayTrack struct {
	Desired       *displayTarget
	Current       *displayCommand
	LastCompleted *displayCommand
	Sequence      uint64
}

func desiredDisplay(st *instanceState) *displayTarget {
	if st.config == nil || st.config.Display == nil || len(st.bindings["local-display"]) != 1 {
		return nil
	}
	mode := displayIdle
	for _, w := range st.windows {
		if w.State == windowMissed {
			mode = displayMissed
			break
		}
		if w.State == windowOpened {
			mode = displayReminder
		}
	}
	raw := st.config.Display.IdleArgs
	switch mode {
	case displayReminder:
		raw = st.config.Display.ReminderArgs
	case displayMissed:
		raw = st.config.Display.MissedArgs
	}
	args, err := canonicalDisplayArgs(raw)
	if err != nil {
		return nil
	} // ConfigureInstance rejects invalid policies.
	return &displayTarget{State: mode, EntityID: st.bindings["local-display"][0], ArgsJSON: args}
}

func sameDisplayTarget(a, b *displayTarget) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Called under the existing operationMu + mu after business state changes or
// a final display receipt. The caller has checked readyForEffects. Repeated
// minute checks and unchanged aggregate states never issue another command.
func (s *Service) displayEffects(instanceID string, st *instanceState) []application.ApplicationEffectUnion {
	target := desiredDisplay(st)
	changed := !sameDisplayTarget(target, st.display.Desired)
	st.display.Desired = target
	var request *application.RequestCommand
	current := st.display.Current
	if target != nil && (current == nil || (current.State != "pending" && current.Target != *target)) {
		st.display.Sequence++
		now := s.now().UTC()
		current = &displayCommand{
			RequestID: fmt.Sprintf("%s%s-%s-%d", displayRequestPrefix, s.runtimeID, instanceID, st.display.Sequence),
			Target:    *target, State: "pending", RequestedAt: now, Deadline: now.Add(displayCommandTimeout),
		}
		st.display.Current = current
		request = &application.RequestCommand{EntityID: target.EntityID, Action: displayAction, ArgsJSON: target.ArgsJSON, IdempotencyKey: current.RequestID, Deadline: optTime(current.Deadline)}
	}
	if !changed && request == nil {
		return nil
	}
	effects := []application.ApplicationEffectUnion{displayRecord(st)}
	if request != nil {
		effects = append(effects, request)
	}
	return effects
}

func (s *Service) onDisplayCompleted(instanceID string, ev *application.RequestCompleted) error {
	state := terminalCommandState(ev.State)
	if state == "" {
		return nil
	}
	s.mu.Lock()
	st := s.instance(instanceID)
	command := st.display.Current
	if command == nil || command.State != "pending" || command.RequestID != ev.RequestID || command.Target.EntityID != ev.EntityID || ev.Action != displayAction {
		s.mu.Unlock()
		return nil
	}
	if err := s.readyForEffects(instanceID, st); err != nil {
		s.mu.Unlock()
		return err
	}
	command.State = state
	command.DoneAt = s.now().UTC()
	command.ResultJSON = ev.ResultJSON
	command.ErrorCode = ev.ErrorCode
	completed := *command
	st.display.LastCompleted = &completed
	// Only a final RequestCompleted releases the single-flight slot. The
	// newest desired state wins; an intermediate queued idle is discarded.
	effects := s.displayEffects(instanceID, st)
	if len(effects) == 0 {
		effects = []application.ApplicationEffectUnion{displayRecord(st)}
	}
	s.mu.Unlock()
	return s.flush(instanceID, effects)
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

func displayRecord(st *instanceState) *application.UpsertDomainRecord {
	return &application.UpsertDomainRecord{RecordType: "display", RecordID: "status", DataJSON: mustJSON(displayData(st)), Version: "1"}
}

func displayData(st *instanceState) map[string]any {
	enabled := st.config != nil && st.config.Display != nil && len(st.bindings["local-display"]) == 1
	target := st.display.Desired
	if !enabled {
		target = nil
	}
	data := map[string]any{
		"title":         "静音视觉提示",
		"summary":       "尚未发出显示命令；仅在显式策略/绑定齐备后的窗口状态变化时发送。",
		"enabled":       enabled,
		"desired_state": "", "desired_entity_id": "", "desired_args": nil, "queued": false,
		"current":        displayCommandData(st.display.Current),
		"last_completed": displayCommandData(st.display.LastCompleted),
	}
	if target != nil {
		data["desired_state"] = target.State
		data["desired_entity_id"] = target.EntityID
		data["desired_args"] = json.RawMessage(target.ArgsJSON)
		data["queued"] = st.display.Current == nil || st.display.Current.Target != *target
	}
	if command := st.display.Current; command != nil {
		wanted := "未启用/未绑定"
		if target != nil {
			wanted = displayStateLabel(target.State)
		}
		summary := fmt.Sprintf("期望提示：%s；当前请求（%s）：%s。取药确认与显示回执分别记录。", wanted, displayStateLabel(command.Target.State), commandOutcomeLabel(command.State))
		if last := st.display.LastCompleted; last != nil && last.RequestID != command.RequestID {
			summary += fmt.Sprintf(" 上一显示请求（%s）：%s。", displayStateLabel(last.Target.State), commandOutcomeLabel(last.State))
		}
		data["summary"] = summary
	}
	return data
}

func displayCommandData(command *displayCommand) any {
	if command == nil {
		return nil
	}
	return map[string]any{
		"request_id": command.RequestID, "visual_state": command.Target.State, "entity_id": command.Target.EntityID,
		"args": json.RawMessage(command.Target.ArgsJSON), "state": command.State,
		"requested_at": optTime(command.RequestedAt), "deadline": optTime(command.Deadline), "done_at": optTime(command.DoneAt),
		"result_json": command.ResultJSON, "error_code": command.ErrorCode,
	}
}

func withDisplayResult(body string, st *instanceState) string {
	if st.display.Current == nil {
		return body
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		return body
	}
	result["display"] = displayData(st)
	return mustJSON(result)
}

func displayStateLabel(state string) string {
	switch state {
	case displayReminder:
		return "待确认"
	case displayMissed:
		return "存在超时未确认窗口"
	case displayIdle:
		return "无待处理窗口"
	default:
		return "未设置"
	}
}

func commandOutcomeLabel(state string) string {
	switch state {
	case "pending":
		return "等待最终回执（不是已显示）"
	case "succeeded":
		return "Core 已报告成功"
	case "failed":
		return "失败"
	case "timedout":
		return "超时（不代表已显示）"
	case "cancelled":
		return "已取消"
	default:
		return "无最终回执"
	}
}
