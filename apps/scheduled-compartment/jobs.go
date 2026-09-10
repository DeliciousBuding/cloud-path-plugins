// SPDX-License-Identifier: Apache-2.0

package scheduledcompartment

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/status"
)

const maxManualMinutes = 120

type jobOutcome struct {
	ArgsJSON   string
	ResultJSON string
}

type startReminderArgs struct {
	CompartmentID string `json:"compartment_id"`
	Minutes       int    `json:"minutes"`
	WindowID      string `json:"window_id"`
}

type windowArgs struct {
	WindowID string `json:"window_id,omitempty"`
}

func jobDescriptors() []application.JobDescriptor {
	return []application.JobDescriptor{
		{ID: jobWindowCheck, Title: "检查到期未确认窗口", InputSchemaJSON: `{"type":"object","additionalProperties":false,"properties":{"window_id":{"type":"string","minLength":1,"title":"窗口 ID（可选）","description":"可留空；自动检查仍扫描全部到期窗口。"}}}`},
		{ID: jobStartReminder, Title: "临时启动取药提醒", ManualOnly: true, InputSchemaJSON: `{"type":"object","additionalProperties":false,"required":["compartment_id","minutes"],"properties":{"compartment_id":{"type":"string","minLength":1,"title":"药格 ID","description":"填写配置 compartments 中的 id，例如 medicine；不是按键实体 ID。"},"minutes":{"type":"integer","minimum":1,"maximum":120,"title":"提醒时长（分钟）","description":"填写 1–120 的整数；截止前需由使用者确认取药。"},"window_id":{"type":"string","minLength":1,"maxLength":128,"title":"本次窗口 ID（可选）","description":"通常留空，由服务端生成唯一编号并在结果中返回；只有需要沿用固定编号时才填写，例如 trial-001。重试请复用同一个 idempotency_key，服务端会返回首次生成的编号。最长 128 个 UTF-8 字节，无首尾空白，不以 schedule: 开头。"}}}`},
		{ID: jobConfirmWindow, Title: "确认指定窗口已取药", ManualOnly: true, InputSchemaJSON: `{"type":"object","additionalProperties":false,"required":["window_id"],"properties":{"window_id":{"type":"string","minLength":1,"title":"要确认的窗口 ID","description":"通常请直接按盒子上的确认按键，无需本操作；只有无法按键时才在这里补记：复制「提醒记录」中的记录编号（窗口 ID）填入。不要填写药格 ID，也不要替换成新窗口 ID。"}}}`},
	}
}

// decodeJobArgs also validates direct SDK callers, not only dashboard requests.
func decodeJobArgs(raw string, dst any) error {
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	if len(raw) > 8192 || !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return fmt.Errorf("job arguments must be a JSON object of at most 8192 bytes")
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("job arguments must contain exactly one JSON object")
	}
	return nil
}

// RunJob keeps window-check automatic. The two user actions are ManualOnly:
// success here means effect submission, NEVER command delivery or ingestion.
func (s *Service) RunJob(_ context.Context, req *application.RunJobRequest) (*application.RunJobResponse, error) {
	if req == nil || strings.TrimSpace(req.PluginInstanceID) == "" {
		return nil, status.Errorf(status.CodeInvalidArgument, "job requires an instance")
	}
	jobID := req.JobID
	var start startReminderArgs
	var target windowArgs
	var canonical string
	var argErr error
	switch {
	case jobID == jobStartReminder:
		argErr = decodeJobArgs(req.ArgsJSON, &start)
		if argErr == nil && (start.Minutes < 1 || start.Minutes > maxManualMinutes) {
			argErr = fmt.Errorf("minutes must be an integer from 1 to %d", maxManualMinutes)
		}
		if argErr == nil && (start.CompartmentID == "" || strings.TrimSpace(start.CompartmentID) != start.CompartmentID) {
			argErr = fmt.Errorf("compartment_id must be a configured, non-empty ID without surrounding whitespace")
		}
		if argErr == nil && start.WindowID != "" && (len(start.WindowID) > maxLocalIDBytes || strings.TrimSpace(start.WindowID) != start.WindowID || strings.HasPrefix(start.WindowID, "schedule:")) {
			argErr = fmt.Errorf("window_id must be 1-128 UTF-8 bytes, have no surrounding whitespace, and not use the reserved schedule: prefix")
		}
		canonical = mustJSON(start)
	case jobID == jobConfirmWindow || jobID == jobWindowCheck || (req.JobType == "scheduled" && strings.HasPrefix(jobID, jobWindowCheck+"-")):
		argErr = decodeJobArgs(req.ArgsJSON, &target)
		if argErr == nil && (strings.TrimSpace(target.WindowID) != target.WindowID || (jobID != jobWindowCheck && target.WindowID == "")) {
			argErr = fmt.Errorf("an exact, non-empty window_id is required")
		}
		if jobID != jobConfirmWindow && jobID != jobWindowCheck {
			// ScheduleTask.ScheduleID is dispatched as JobID by the public Host.
			if jobID != windowTaskID(target.WindowID) {
				argErr = fmt.Errorf("scheduled check ID must match its window_id")
			}
			jobID = jobWindowCheck
		}
		canonical = mustJSON(target)
	default:
		return nil, status.Errorf(status.CodeUnimplemented, "job %q is not implemented", req.JobID)
	}
	if argErr != nil {
		return nil, status.Errorf(status.CodeInvalidArgument, "%v", argErr)
	}

	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, status.Errorf(status.CodeUnavailable, "plugin is shutting down")
	}
	st := s.instance(req.PluginInstanceID)
	if st.deliveryUncertain {
		s.mu.Unlock()
		return nil, status.Errorf(status.CodeUnavailable, "previous effect delivery is uncertain; reconcile instance records before further actions")
	}
	cacheKey := req.JobID + "\x00" + req.IdempotencyKey
	if req.IdempotencyKey != "" {
		if prev, ok := st.jobs[cacheKey]; ok {
			s.mu.Unlock()
			if prev.ArgsJSON != canonical {
				return nil, status.Errorf(status.CodeInvalidArgument, "idempotency key was already used with different arguments")
			}
			return &application.RunJobResponse{JobID: req.JobID, Status: status.New(), ResultJSON: prev.ResultJSON}, nil
		}
	}
	now := s.now().UTC()
	var effects []application.ApplicationEffectUnion
	var body string
	var err error
	switch jobID {
	case jobStartReminder:
		if err = s.readyForEffects(req.PluginInstanceID, st); err == nil {
			if !st.config.hasCompartment(start.CompartmentID) {
				err = status.Errorf(status.CodeInvalidArgument, "unknown compartment_id %q", start.CompartmentID)
			} else {
				// window_id is optional. The canonical idempotency args above keep
				// the original empty value, so a retry with the same key returns the
				// cached result (with the generated id) instead of opening a second
				// window. Only a cache miss reaches this generation.
				windowID := start.WindowID
				if windowID == "" {
					windowID = s.newManualWindowID(st)
				}
				if w := st.windows[windowID]; w != nil {
					if w.Source != sourceManual || w.Compartment != start.CompartmentID || w.End.Sub(w.Start) != time.Duration(start.Minutes)*time.Minute {
						err = status.Errorf(status.CodeInvalidArgument, "window_id %q was already used for a different window", windowID)
					} else {
						body = windowResult(w)
					}
				} else {
					w := &windowTrack{ID: windowID, Source: sourceManual, Compartment: start.CompartmentID, Start: now, End: now.Add(time.Duration(start.Minutes) * time.Minute)}
					effects = s.startWindow(st, w, now)
					body = windowResult(w)
				}
			}
		}
	case jobConfirmWindow:
		if err = s.readyForEffects(req.PluginInstanceID, st); err == nil {
			w := st.windows[target.WindowID]
			if w == nil {
				err = status.Errorf(status.CodeNotFound, "unknown window_id %q; no window was confirmed", target.WindowID)
			} else if now.Before(w.Start) || (!w.OpenedAt.IsZero() && now.Before(w.OpenedAt)) {
				err = status.Errorf(status.CodeFailedPrecondition, "window has not opened at the current clock time")
			} else {
				effects = confirmWindow(w, now, "dashboard")
				body = windowResult(w)
			}
		}
	case jobWindowCheck:
		// Core calls this once a minute. Open today's due daily windows first,
		// then preserve the original full expiry scan. Expiry is [start, end).
		due, dueErr := s.openDueWindows(req.PluginInstanceID, st, now)
		if dueErr != nil {
			err = dueErr
			break
		}
		effects = append(effects, due...)
		missed := []string{}
		for id, w := range st.windows {
			if w.State == windowOpened && !now.Before(w.End) {
				missed = append(missed, id)
			}
		}
		sort.Strings(missed)
		if len(missed) != 0 {
			err = s.readyForEffects(req.PluginInstanceID, st)
		}
		if err == nil {
			for _, id := range missed {
				w := st.windows[id]
				markMissed(w)
				effects = append(effects, windowRecord(w), cancelTaskEffect(w.ID), missedNotificationEffect(w))
			}
			body = resultJSON(missed)
		}
	}
	if err == nil {
		if len(effects) != 0 {
			effects = append(effects, s.displayEffects(req.PluginInstanceID, st)...)
		}
		body = withDisplayResult(body, st)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := s.flush(req.PluginInstanceID, effects); err != nil {
		return nil, err // never cache a successful job before its effects are sent
	}
	if req.IdempotencyKey != "" {
		s.mu.Lock()
		st.jobs[cacheKey] = jobOutcome{ArgsJSON: canonical, ResultJSON: body}
		s.mu.Unlock()
	}
	return &application.RunJobResponse{JobID: req.JobID, Status: status.New(), ResultJSON: body}, nil
}

// Called with s.mu held, BEFORE a state transition. The SDK has no effect ACK
// or record-rehydration API; a failed batch cannot be pretended to have applied.
func (s *Service) readyForEffects(instanceID string, st *instanceState) error {
	if s.closed || st.deliveryUncertain {
		return status.Errorf(status.CodeUnavailable, "instance is stopped or previous effect delivery is uncertain")
	}
	if st.config == nil || len(st.bindings["compartments"]) != len(st.config.Compartments) {
		return status.Errorf(status.CodeFailedPrecondition, "configure the instance and validate exactly one key binding per configured compartment first")
	}
	if s.writers[instanceID] == nil {
		return status.Errorf(status.CodeUnavailable, "instance has no active event/effect stream; no action was applied")
	}
	return nil
}

// openDueWindows restores the minute-loop schedule owner used by the public
// Host: Core calls window-check every minute, and the app opens each due daily
// window once. ScheduleTick remains wire-compatible for older hosts.
func (s *Service) openDueWindows(instanceID string, st *instanceState, now time.Time) ([]application.ApplicationEffectUnion, error) {
	if st == nil || st.config == nil {
		return nil, nil
	}
	loc, err := time.LoadLocation(st.config.Timezone)
	if err != nil {
		return nil, nil
	}
	local := now.In(loc)
	var effects []application.ApplicationEffectUnion
	for _, spec := range st.config.Schedule {
		startClock, startOK := parseHHMM(spec.Start)
		endClock, endOK := parseHHMM(spec.End)
		if !startOK || !endOK {
			continue
		}
		start := time.Date(local.Year(), local.Month(), local.Day(), startClock.Hour(), startClock.Minute(), 0, 0, loc)
		end := time.Date(local.Year(), local.Month(), local.Day(), endClock.Hour(), endClock.Minute(), 0, 0, loc)
		if !end.After(start) || now.Before(start) || !now.Before(end) {
			continue
		}
		id := scheduleOccurrenceID(spec.ID, start)
		if st.windows[id] != nil {
			continue
		}
		if err := s.readyForEffects(instanceID, st); err != nil {
			return nil, err
		}
		w := &windowTrack{ID: id, ScheduleID: spec.ID, Source: sourceSchedule, Compartment: spec.Compartment, Start: start, End: end}
		effects = append(effects, s.startWindow(st, w, now)...)
	}
	return effects, nil
}

// startWindow is shared by real ScheduleTicks and the manual start Job.
// Compartment/key identity is captured so reconfiguration cannot reinterpret
// an old window's physical confirmation as a different compartment.
func (s *Service) startWindow(st *instanceState, w *windowTrack, now time.Time) []application.ApplicationEffectUnion {
	w.ReminderEntity = reminderEntity(st)
	for i, cp := range st.config.Compartments {
		if cp.ID == w.Compartment {
			w.KeyEntity = st.bindings["compartments"][i]
			w.CompartmentName = cp.Name
			break
		}
	}
	st.windows[w.ID] = w
	if !now.Before(w.End) {
		w.ReminderState = "not_requested"
		markMissed(w)
		return []application.ApplicationEffectUnion{windowRecord(w), missedNotificationEffect(w)}
	}
	w.State = windowOpened
	w.OpenedAt = now
	if reminderSilenced(st) {
		w.ReminderState = reminderSuppressed
	} else {
		w.ReminderState = "pending"
	}
	return s.windowStartEffects(st, w)
}

func confirmWindow(w *windowTrack, at time.Time, source string) []application.ApplicationEffectUnion {
	if w.State == windowCompleted || w.State == windowCompletedLate || at.Before(w.Start) || (!w.OpenedAt.IsZero() && at.Before(w.OpenedAt)) {
		return nil
	}
	w.State = windowCompleted
	if !at.Before(w.End) {
		w.State = windowCompletedLate
		w.MissedAt = w.End
	}
	w.ClosedAt = at
	w.ConfirmedAt = at
	w.ConfirmationSource = source
	return []application.ApplicationEffectUnion{windowRecord(w), cancelTaskEffect(w.ID)}
}

func markMissed(w *windowTrack) {
	w.State = windowMissed
	w.MissedAt = w.End
	w.ClosedAt = w.End
}

func windowResult(w *windowTrack) string {
	title, summary := windowPresentation(w)
	return mustJSON(map[string]any{
		"title": title, "summary": summary,
		"window_id": w.ID, "compartment_id": w.Compartment, "source": w.Source,
		"state": w.State, "start": optTime(w.Start), "end": optTime(w.End),
		"reminder_request_id": reminderRequestID(w), "reminder_state": w.ReminderState,
		"confirmation_source": w.ConfirmationSource, "confirmed_at": optTime(w.ConfirmedAt),
		"effects_status": "submitted",
	})
}

func windowTaskID(id string) string { return jobWindowCheck + "-" + id }

// newManualWindowID mints a server-generated window id for start-reminder
// callers that omit window_id. The "manual-" prefix keeps it clear of the
// reserved "schedule:" occurrence namespace, and any collision with a live
// window forces a fresh draw.
func (s *Service) newManualWindowID(st *instanceState) string {
	for {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return fmt.Sprintf("manual-%d", s.now().UnixNano())
		}
		id := "manual-" + hex.EncodeToString(random[:])
		if st.windows[id] == nil {
			return id
		}
	}
}

// Core reuses the daily spec ID. A date/instant-qualified ID keeps tomorrow's
// window independent and prevents yesterday's dashboard retry confirming it.
func scheduleOccurrenceID(id string, start time.Time) string {
	return "schedule:" + id + ":" + start.UTC().Format("20060102T150405.999999999Z")
}

func reminderRequestID(w *windowTrack) string {
	if w.ReminderState == "not_requested" || w.ReminderState == reminderSuppressed || w.ReminderEntity == "" {
		return ""
	}
	return reminderRequestPrefix + w.ID
}

// compartmentDisplayName is the user-facing name captured when the window
// started. Later config edits never rename historical confirmations.
func compartmentDisplayName(w *windowTrack) string {
	if name := strings.TrimSpace(w.CompartmentName); name != "" {
		return name
	}
	return w.Compartment
}

// Presentation belongs to the application, not Core or a device/ACK adapter.
// Names are captured when a window starts so later config edits cannot rename
// historical confirmations. Command results never decide the collection text.
func windowPresentation(w *windowTrack) (string, string) {
	name := compartmentDisplayName(w)
	switch w.State {
	case windowOpened:
		if w.ReminderState == reminderSuppressed {
			return name + "：待确认取药", "提醒窗口已开启（静音策略：未发送蜂鸣命令），等待使用者确认取药；可视提示结果另行记录。"
		}
		return name + "：待确认取药", "提醒窗口已开启，等待使用者确认取药；提示命令的结果另行记录。"
	case windowCompleted:
		return name + "：已人工确认取药", "使用者已在窗口内确认取药，不代表药物已经吞服。"
	case windowMissed:
		return name + "：已超时", "窗口已到期，尚未收到按时取药确认；超时不等于已证明漏服。"
	case windowCompletedLate:
		return name + "：迟到确认取药", "使用者已在截止时或之后确认取药，原超时时间保留；不代表药物已经吞服。"
	default:
		return name + "：取药窗口", "取药确认与提示命令的执行结果分别记录。"
	}
}
