package musicplayer

import (
	"fmt"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
)

func musicSessionRecord(st *instanceState) *application.UpsertDomainRecord {
	return &application.UpsertDomainRecord{
		RecordType: sessionRecordType,
		RecordID:   sessionRecordID,
		DataJSON:   mustJSON(sessionData(st)),
		Version:    sessionRecordVer,
	}
}

func sessionData(st *instanceState) map[string]any {
	soundBound := soundEntity(st) != ""
	displayBound := st != nil && len(st.bindings[displayRequirement]) == 1
	indicatorBound := st != nil && len(st.bindings[indicatorRequirement]) == 1

	data := map[string]any{
		"title":                    "音乐播放器",
		"summary":                  "尚未创建音乐会话。",
		"song":                     "",
		"status":                   statusIdle,
		"queued_at":                "",
		"last_note":                nil,
		"repeat":                   0,
		"request_id":               "",
		"total_notes":              0,
		"completed_notes":          0,
		"error_code":               "",
		"result_json":              "",
		"failed_note":              nil,
		"sound_bound":              soundBound,
		"local_display_bound":      displayBound,
		"indicator_bound":          indicatorBound,
		"degraded":                 !displayBound || !indicatorBound,
		"runtime_state_persistent": false,
	}

	if st == nil || st.session == nil {
		return data
	}
	session := st.session
	data["song"] = session.Song
	data["status"] = session.Status
	data["queued_at"] = session.QueuedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
	data["repeat"] = session.Repeat
	data["request_id"] = session.RequestID
	data["total_notes"] = session.TotalNotes
	data["completed_notes"] = session.CompletedNotes
	data["error_code"] = session.ErrorCode
	data["result_json"] = session.ResultJSON
	if session.LastNote != nil {
		data["last_note"] = session.LastNote
	}
	if session.FailedNote != nil {
		data["failed_note"] = session.FailedNote
	}
	data["summary"] = sessionSummary(session, displayBound, indicatorBound)
	return data
}

func sessionSummary(session *musicSession, displayBound, indicatorBound bool) string {
	optional := ""
	if !displayBound && !indicatorBound {
		optional = " 可选显示与指示灯均未绑定，已按纯声音模式降级。"
	} else if !displayBound {
		optional = " 本地显示未绑定，已降级。"
	} else if !indicatorBound {
		optional = " 指示灯未绑定，已降级。"
	}
	switch session.Status {
	case statusQueued:
		return fmt.Sprintf("已排队，共 %d 个音符；等待设备回执。%s", session.TotalNotes, optional)
	case statusPlaying:
		return fmt.Sprintf("正在播放，已完成 %d/%d 个音符。%s", session.CompletedNotes, session.TotalNotes, optional)
	case statusCompleted:
		return fmt.Sprintf("已完成播放，共 %d 个音符。%s", session.TotalNotes, optional)
	case statusFailed:
		return fmt.Sprintf("播放失败，已完成 %d/%d 个音符。%s", session.CompletedNotes, session.TotalNotes, optional)
	default:
		return "尚未创建音乐会话。"
	}
}

func requestCommand(entityID string, command *sessionCommand) *application.RequestCommand {
	return &application.RequestCommand{
		EntityID:       entityID,
		Action:         command.Action,
		ArgsJSON:       command.ArgsJSON,
		IdempotencyKey: command.Key,
	}
}

func (s *Service) onRequestCompleted(instanceID string, event *application.RequestCompleted) error {
	if event == nil {
		return nil
	}

	s.mu.Lock()
	st := s.instanceLocked(instanceID)
	session := st.session
	if session == nil || session.Status == statusCompleted || session.Status == statusFailed {
		s.mu.Unlock()
		return nil
	}
	if session.NextIndex <= 0 || session.NextIndex > len(session.CommandOrder) {
		s.mu.Unlock()
		return nil
	}
	// Only the one in-flight note may complete. This prevents an out-of-order
	// or replayed future completion from skipping notes in the sequence.
	if event.RequestID != session.CommandOrder[session.NextIndex-1] {
		s.mu.Unlock()
		return nil
	}
	command := session.Commands[event.RequestID]
	if command == nil || command.State != commandQueued {
		s.mu.Unlock()
		return nil
	}
	if event.EntityID != "" && event.EntityID != soundEntity(st) {
		s.mu.Unlock()
		return nil
	}
	if event.Action != "" && event.Action != command.Action {
		s.mu.Unlock()
		return nil
	}
	state := terminalCommandState(event.State)
	if state == "" {
		s.mu.Unlock()
		return nil
	}

	command.State = state
	switch state {
	case commandSucceeded:
		last := command.Notes[len(command.Notes)-1]
		session.CompletedNotes += len(command.Notes)
		session.LastNote = &NoteResult{
			Index:       command.StartNoteIndex + len(command.Notes) - 1,
			FrequencyHz: last.FrequencyHz,
			DurationMS:  last.DurationMS,
		}
		if session.CompletedNotes >= session.TotalNotes {
			session.Status = statusCompleted
		} else {
			session.Status = statusPlaying
		}
	case commandFailed, commandTimedOut, commandCancelled:
		first := command.Notes[0]
		session.Status = statusFailed
		session.ErrorCode = event.ErrorCode
		session.ResultJSON = event.ResultJSON
		session.FailedNote = &NoteResult{
			Index:       command.StartNoteIndex,
			FrequencyHz: first.FrequencyHz,
			DurationMS:  first.DurationMS,
		}
		// Fail closed: once a command reaches a non-success terminal state, no
		// later command may be emitted even if another event reaches this path.
		session.NextIndex = len(session.CommandOrder) + 1
	default:
		s.mu.Unlock()
		return nil
	}
	effects := []application.ApplicationEffectUnion{musicSessionRecord(st)}
	if state == commandSucceeded && session.Status == statusPlaying && session.NextIndex < len(session.CommandOrder) {
		next := session.Commands[session.CommandOrder[session.NextIndex]]
		session.NextIndex++
		effects = append(effects, requestCommand(soundEntity(st), next))
	}
	s.mu.Unlock()

	return s.sendEffects(instanceID, effects)
}
