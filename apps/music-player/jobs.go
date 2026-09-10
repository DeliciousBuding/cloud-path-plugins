package musicplayer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/status"
)

const (
	// 一首歌一个 job：UI 会把无参数操作直接渲染成按钮，按钮文案就是曲名，
	// 普通用户打开页面即可看到「播放小星星」这类可点动作，而不是一个下拉框。
	jobPlayLittleStar = "play-little-star"
	jobPlayBirthday   = "play-birthday"
	jobPlayOdeToJoy   = "play-ode-to-joy"
	jobPlayNote       = "play-note"
	jobStatus         = "status"

	maxJobArgsBytes = 8192
)

// songForJob 把每首歌的 job 映射到内置曲目 key。
var songForJob = map[string]string{
	jobPlayLittleStar: songLittleStar,
	jobPlayBirthday:   songBirthday,
	jobPlayOdeToJoy:   songOdeToJoy,
}

type playSongArgs struct {
	Repeat int `json:"repeat"`
}

type playNoteArgs struct {
	FrequencyHz int `json:"frequency_hz"`
	DurationMS  int `json:"duration_ms"`
}

type playSequenceArgs struct {
	Notes []Note `json:"notes"`
	GapMS int    `json:"gap_ms,omitempty"`
}

type emptyArgs struct{}

// songJobDescriptor 声明一个无参数（可带可选 repeat）的曲目按钮。
// 无 required 字段意味着前端会把它渲染成直接可见的按钮，而不是折叠表单。
func songJobDescriptor(id, title string) application.JobDescriptor {
	return application.JobDescriptor{
		ID:              id,
		Title:           title,
		ManualOnly:      true,
		InputSchemaJSON: `{"type":"object","additionalProperties":false,"properties":{"repeat":{"type":"integer","minimum":1,"maximum":3,"default":1,"title":"重复次数"}}}`,
	}
}

func jobDescriptors() []application.JobDescriptor {
	return []application.JobDescriptor{
		songJobDescriptor(jobPlayLittleStar, "播放小星星"),
		songJobDescriptor(jobPlayBirthday, "播放生日歌"),
		songJobDescriptor(jobPlayOdeToJoy, "播放欢乐颂"),
		{
			ID:              jobPlayNote,
			Title:           "播放单音（Hz / ms）",
			ManualOnly:      true,
			InputSchemaJSON: `{"type":"object","additionalProperties":false,"required":["frequency_hz","duration_ms"],"properties":{"frequency_hz":{"type":"integer","minimum":1,"maximum":4000,"title":"频率 Hz"},"duration_ms":{"type":"integer","minimum":10,"maximum":1200,"multipleOf":10,"title":"时长 ms"}}}`,
		},
		{
			ID:              jobStatus,
			Title:           "刷新播放状态",
			ManualOnly:      true,
			InputSchemaJSON: `{"type":"object","additionalProperties":false,"properties":{}}`,
		},
	}
}

func decodeStrictArgs(raw string, dst any) error {
	text := strings.TrimSpace(raw)
	if text == "" {
		text = "{}"
	}
	if len(text) > maxJobArgsBytes {
		return fmt.Errorf("job arguments must be at most %d bytes", maxJobArgsBytes)
	}
	if !strings.HasPrefix(text, "{") {
		return fmt.Errorf("job arguments must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
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

func (s *Service) RunJob(ctx context.Context, req *application.RunJobRequest) (*application.RunJobResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil || strings.TrimSpace(req.PluginInstanceID) == "" {
		return nil, status.Errorf(status.CodeInvalidArgument, "job requires an instance")
	}
	if len(req.IdempotencyKey) > maxIdempotencyKeyBytes {
		return nil, status.Errorf(status.CodeInvalidArgument, "idempotency_key must be at most %d bytes", maxIdempotencyKeyBytes)
	}
	if req.Deadline != "" {
		deadline, err := time.Parse(time.RFC3339Nano, req.Deadline)
		if err != nil {
			return nil, status.Errorf(status.CodeInvalidArgument, "invalid job deadline")
		}
		if !s.now().Before(deadline) {
			return nil, status.Errorf(status.CodeDeadlineExceeded, "job deadline expired")
		}
	}

	var (
		canonical string
		notes     []Note
		song      string
		repeat    int
	)
	switch req.JobID {
	case jobPlayLittleStar, jobPlayBirthday, jobPlayOdeToJoy:
		var args playSongArgs
		if err := decodeStrictArgs(req.ArgsJSON, &args); err != nil {
			return nil, status.Errorf(status.CodeInvalidArgument, "invalid playback arguments: %v", err)
		}
		repeat = args.Repeat
		if repeat == 0 {
			repeat = 1
		}
		if repeat < 1 || repeat > 3 {
			return nil, status.Errorf(status.CodeInvalidArgument, "repeat must be an integer from 1 to 3")
		}
		var err error
		song = songForJob[req.JobID]
		notes, err = notesForSong(song)
		if err != nil {
			return nil, status.Errorf(status.CodeInvalidArgument, "unknown song %q", song)
		}
		canonical = mustJSON(playSongArgs{Repeat: repeat})
	case jobPlayNote:
		var args playNoteArgs
		if err := decodeStrictArgs(req.ArgsJSON, &args); err != nil {
			return nil, status.Errorf(status.CodeInvalidArgument, "invalid play-note arguments: %v", err)
		}
		note := Note{FrequencyHz: args.FrequencyHz, DurationMS: args.DurationMS}
		if err := validateNote(note); err != nil {
			return nil, status.Errorf(status.CodeInvalidArgument, "invalid play-note arguments: %v", err)
		}
		notes = []Note{note}
		song = "custom"
		repeat = 1
		canonical = mustJSON(args)
	case jobStatus:
		var args emptyArgs
		if err := decodeStrictArgs(req.ArgsJSON, &args); err != nil {
			return nil, status.Errorf(status.CodeInvalidArgument, "invalid status arguments: %v", err)
		}
		canonical = "{}"
	default:
		return nil, status.Errorf(status.CodeUnimplemented, "unknown job %q", req.JobID)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, status.Errorf(status.CodeUnavailable, "plugin is shutting down")
	}
	st := s.instanceLocked(req.PluginInstanceID)
	if !st.configured {
		s.mu.Unlock()
		return nil, status.Errorf(status.CodeFailedPrecondition, "instance is not configured")
	}
	cacheKey := req.IdempotencyKey
	if cacheKey != "" {
		if previous, ok := st.jobs[cacheKey]; ok {
			s.mu.Unlock()
			if previous.JobID != req.JobID || previous.ArgsJSON != canonical {
				return nil, status.Errorf(status.CodeInvalidArgument, "idempotency_key was already used with different job arguments")
			}
			return &application.RunJobResponse{JobID: req.JobID, Status: status.New(), ResultJSON: previous.ResultJSON}, nil
		}
	}

	var effects []application.ApplicationEffectUnion
	var body string

	if req.JobID == jobStatus {
		body = mustJSON(sessionData(st))
	} else {
		if hasActiveSession(st) {
			s.mu.Unlock()
			return nil, status.Errorf(status.CodeFailedPrecondition, "a music session is already queued or playing")
		}
		sound := soundEntity(st)
		if !st.bindingsValid || sound == "" {
			s.mu.Unlock()
			return nil, status.Errorf(status.CodeFailedPrecondition, "bind exactly one sound requirement before playing")
		}
		if s.writerForLocked(req.PluginInstanceID) == nil {
			s.mu.Unlock()
			return nil, status.Errorf(status.CodeUnavailable, "instance has no active event/effect stream")
		}

		now := s.now().UTC()
		requestID := s.requestIDLocked(req.IdempotencyKey)
		session := &musicSession{
			RequestID:  requestID,
			JobID:      req.JobID,
			Song:       song,
			Status:     statusQueued,
			QueuedAt:   now,
			Repeat:     repeat,
			TotalNotes: len(notes) * repeat,
			Commands:   map[string]*sessionCommand{},
		}
		for repetition := 0; repetition < repeat; repetition++ {
			startNoteIndex := repetition*len(notes) + 1
			commandNotes := append([]Note(nil), notes...)
			action := toneSequenceAction
			argsJSON := mustJSON(playSequenceArgs{Notes: commandNotes})
			key := requestID + ":seq:" + strconv.Itoa(repetition+1)
			if req.JobID == jobPlayNote {
				action = toneAction
				argsJSON = mustJSON(commandNotes[0])
				key = requestID + ":note:1"
			}
			session.CommandOrder = append(session.CommandOrder, key)
			session.Commands[key] = &sessionCommand{
				Key:            key,
				Action:         action,
				ArgsJSON:       argsJSON,
				Notes:          commandNotes,
				StartNoteIndex: startNoteIndex,
				State:          commandQueued,
			}
		}
		st.session = session
		body = mustJSON(sessionData(st))
		effects = append(effects, musicSessionRecord(st))
		if len(session.CommandOrder) == 0 {
			s.mu.Unlock()
			return nil, status.Errorf(status.CodeFailedPrecondition, "song has no notes")
		}
		session.NextIndex = 1
		effects = append(effects, requestCommand(sound, session.Commands[session.CommandOrder[0]]))
	}
	s.mu.Unlock()

	if err := s.sendEffects(req.PluginInstanceID, effects); err != nil {
		return nil, err
	}

	if cacheKey != "" {
		s.mu.Lock()
		if len(st.jobOrder) >= maxJobResults {
			delete(st.jobs, st.jobOrder[0])
			st.jobOrder = st.jobOrder[1:]
		}
		st.jobOrder = append(st.jobOrder, cacheKey)
		st.jobs[cacheKey] = jobOutcome{JobID: req.JobID, ArgsJSON: canonical, ResultJSON: body}
		s.mu.Unlock()
	}
	return &application.RunJobResponse{JobID: req.JobID, Status: status.New(), ResultJSON: body}, nil
}

func (s *Service) requestIDLocked(idempotencyKey string) string {
	if idempotencyKey != "" {
		return "music-" + idempotencyKey
	}
	return fmt.Sprintf("music-%d-%d", s.now().UTC().UnixNano(), s.idCounter.Add(1))
}
