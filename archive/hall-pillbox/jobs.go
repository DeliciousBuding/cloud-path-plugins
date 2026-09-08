package hallpillbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/status"
)

type jobOutcome struct {
	ArgsJSON   string
	ResultJSON string
}

type startWindowArgs struct {
	WindowID string `json:"window_id"`
	Minutes  int    `json:"minutes"`
}

type confirmWindowArgs struct {
	WindowID string `json:"window_id"`
	Source   string `json:"source"`
}

type checkWindowArgs struct {
	WindowID string `json:"window_id,omitempty"`
}

type statusArgs struct {
	WindowID string `json:"window_id,omitempty"`
}

func jobDescriptors() []application.JobDescriptor {
	return []application.JobDescriptor{
		{ID: jobStartWindow, Title: "启动药盒提醒窗口", ManualOnly: true, InputSchemaJSON: `{"type":"object","additionalProperties":false,"required":["window_id","minutes"],"properties":{"window_id":{"type":"string","minLength":1,"maxLength":128,"title":"窗口 ID","description":"本次提醒窗口的唯一标识，例如 trial-001；重试复用同一个。"},"minutes":{"type":"integer","minimum":1,"maximum":120,"title":"提醒时长（分钟）","description":"填写 1-120 的整数。"}}}`},
		{ID: jobConfirmWindow, Title: "管理台确认窗口", ManualOnly: true, InputSchemaJSON: `{"type":"object","additionalProperties":false,"required":["window_id","source"],"properties":{"window_id":{"type":"string","minLength":1,"maxLength":128,"title":"窗口 ID","description":"要确认的窗口 ID。"},"source":{"type":"string","enum":["dashboard"],"title":"确认来源","description":"管理台确认必须填写 dashboard。"}}}`},
		{ID: jobCheckWindow, Title: "检查到期未确认窗口", InputSchemaJSON: `{"type":"object","additionalProperties":false,"properties":{"window_id":{"type":"string","minLength":1,"maxLength":128,"title":"窗口 ID（可选）","description":"可留空；留空时扫描全部到期窗口。"}}}`},
		{ID: jobStatus, Title: "查询药盒窗口状态", ManualOnly: true, InputSchemaJSON: `{"type":"object","additionalProperties":false,"properties":{"window_id":{"type":"string","minLength":1,"maxLength":128,"title":"窗口 ID（可选）","description":"可留空；留空时返回最近窗口。"}}}`},
	}
}

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

func validWindowID(id string) bool {
	return id != "" && len(id) <= maxLocalIDBytes && strings.TrimSpace(id) == id && !strings.HasPrefix(id, "schedule:")
}

func (s *Service) RunJob(_ context.Context, req *application.RunJobRequest) (*application.RunJobResponse, error) {
	if req == nil || strings.TrimSpace(req.PluginInstanceID) == "" {
		return nil, status.Errorf(status.CodeInvalidArgument, "job requires an instance")
	}

	jobID := req.JobID
	var canonical string
	var start startWindowArgs
	var confirm confirmWindowArgs
	var check checkWindowArgs
	var statusArgs statusArgs
	var argErr error

	switch jobID {
	case jobStartWindow:
		argErr = decodeJobArgs(req.ArgsJSON, &start)
		if argErr == nil && !validWindowID(start.WindowID) {
			argErr = fmt.Errorf("window_id must be 1-128 UTF-8 bytes, have no surrounding whitespace, and not use the reserved schedule: prefix")
		}
		if argErr == nil && (start.Minutes < 1 || start.Minutes > maxManualMinutes) {
			argErr = fmt.Errorf("minutes must be an integer from 1 to %d", maxManualMinutes)
		}
		canonical = mustJSON(start)
	case jobConfirmWindow:
		argErr = decodeJobArgs(req.ArgsJSON, &confirm)
		if argErr == nil && !validWindowID(confirm.WindowID) {
			argErr = fmt.Errorf("window_id must be 1-128 UTF-8 bytes with no surrounding whitespace")
		}
		if argErr == nil && confirm.Source != sourceDashboard {
			argErr = fmt.Errorf("source must be %q", sourceDashboard)
		}
		canonical = mustJSON(confirm)
	case jobCheckWindow:
		argErr = decodeJobArgs(req.ArgsJSON, &check)
		if argErr == nil && check.WindowID != "" && !validWindowID(check.WindowID) {
			argErr = fmt.Errorf("window_id must be 1-128 UTF-8 bytes with no surrounding whitespace")
		}
		canonical = mustJSON(check)
	case jobStatus:
		argErr = decodeJobArgs(req.ArgsJSON, &statusArgs)
		if argErr == nil && statusArgs.WindowID != "" && !validWindowID(statusArgs.WindowID) {
			argErr = fmt.Errorf("window_id must be 1-128 UTF-8 bytes with no surrounding whitespace")
		}
		canonical = mustJSON(statusArgs)
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
	case jobStartWindow:
		if err = s.readyForEffects(req.PluginInstanceID, st); err == nil {
			if w := st.windows[start.WindowID]; w != nil {
				if w.Source != sourceManual || w.Compartment != st.config.resolvedCompartment() || w.End.Sub(w.Start) != time.Duration(start.Minutes)*time.Minute {
					err = status.Errorf(status.CodeInvalidArgument, "window_id %q was already used for a different window", start.WindowID)
				} else {
					body = windowResult(w)
				}
			} else {
				w := &windowTrack{
					ID:          start.WindowID,
					Source:      sourceManual,
					Compartment: st.config.resolvedCompartment(),
					Start:       now,
					End:         now.Add(time.Duration(start.Minutes) * time.Minute),
				}
				effects = s.startWindow(st, w, now)
				body = windowResult(w)
			}
		}
	case jobConfirmWindow:
		if err = s.readyForEffects(req.PluginInstanceID, st); err == nil {
			w := st.windows[confirm.WindowID]
			if w == nil {
				err = status.Errorf(status.CodeNotFound, "unknown window_id %q; no window was confirmed", confirm.WindowID)
			} else if now.Before(w.Start) {
				err = status.Errorf(status.CodeFailedPrecondition, "window has not opened at the current clock time")
			} else {
				effects = s.confirmWindowEffects(st, w, now, sourceDashboard)
				body = windowResult(w)
			}
		}
	case jobCheckWindow:
		missed := make([]string, 0)
		for id, w := range st.windows {
			if check.WindowID != "" && id != check.WindowID {
				continue
			}
			if w.State == windowOpened && !now.Before(w.End) {
				missed = append(missed, id)
			}
		}
		if check.WindowID != "" && st.windows[check.WindowID] == nil {
			err = status.Errorf(status.CodeNotFound, "unknown window_id %q", check.WindowID)
		} else {
			sort.Strings(missed)
			if len(missed) != 0 {
				err = s.readyForEffects(req.PluginInstanceID, st)
			}
			if err == nil {
				for _, id := range missed {
					effects = append(effects, s.markMissed(st, st.windows[id])...)
				}
				body = resultJSON(missed)
			}
		}
	case jobStatus:
		if statusArgs.WindowID == "" {
			body = mustJSON(statusData(st))
		} else if w := st.windows[statusArgs.WindowID]; w == nil {
			err = status.Errorf(status.CodeNotFound, "unknown window_id %q", statusArgs.WindowID)
		} else {
			body = mustJSON(map[string]any{"window": windowData(w)})
		}
	}

	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := s.flush(req.PluginInstanceID, effects); err != nil {
		return nil, err
	}
	if req.IdempotencyKey != "" {
		s.mu.Lock()
		st.jobs[cacheKey] = jobOutcome{ArgsJSON: canonical, ResultJSON: body}
		s.mu.Unlock()
	}
	return &application.RunJobResponse{JobID: req.JobID, Status: status.New(), ResultJSON: body}, nil
}
