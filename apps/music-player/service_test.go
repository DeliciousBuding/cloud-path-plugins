package musicplayer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/status"
)

const testInstance = "music-instance"

var testBindings = []application.Binding{
	{RequirementID: soundRequirement, EntityID: "entity/buzzer"},
}

type fakeEventReader struct {
	events chan *application.ApplicationEvent
}

func newFakeEventReader() *fakeEventReader {
	return &fakeEventReader{events: make(chan *application.ApplicationEvent, 32)}
}

func (r *fakeEventReader) Recv(ctx context.Context) (*application.ApplicationEvent, error) {
	select {
	case event, ok := <-r.events:
		if !ok {
			return nil, io.EOF
		}
		return event, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *fakeEventReader) send(event *application.ApplicationEvent) {
	r.events <- event
}

type fakeEffectWriter struct {
	mu      sync.Mutex
	effects []*application.ApplicationEffect
	notify  chan struct{}
}

func newFakeEffectWriter() *fakeEffectWriter {
	return &fakeEffectWriter{notify: make(chan struct{})}
}

func (w *fakeEffectWriter) Send(_ context.Context, effect *application.ApplicationEffect) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.effects = append(w.effects, effect)
	close(w.notify)
	w.notify = make(chan struct{})
	return nil
}

func (w *fakeEffectWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.effects)
}

func (w *fakeEffectWriter) snapshot() []*application.ApplicationEffect {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]*application.ApplicationEffect, len(w.effects))
	copy(out, w.effects)
	return out
}

func (w *fakeEffectWriter) waitFor(t *testing.T, want int) []*application.ApplicationEffect {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		w.mu.Lock()
		if len(w.effects) >= want {
			out := make([]*application.ApplicationEffect, len(w.effects))
			copy(out, w.effects)
			w.mu.Unlock()
			return out
		}
		notify := w.notify
		w.mu.Unlock()
		select {
		case <-notify:
		case <-time.After(time.Until(deadline)):
			t.Fatalf("timed out waiting for %d effects; got %d", want, w.count())
		}
	}
}

type harness struct {
	t      *testing.T
	svc    *Service
	reader *fakeEventReader
	writer *fakeEffectWriter
	ctx    context.Context
	cancel context.CancelFunc
	done   chan error
	seq    uint64
}

func newHarness(t *testing.T, bindings []application.Binding) *harness {
	t.Helper()
	svc := New()
	svc.now = func() time.Time {
		return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	}
	configured, err := svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{
		PluginInstanceID: testInstance,
		Config:           []byte(`{}`),
		ConfigRevision:   1,
	})
	if err != nil || !configured.Status.IsOK() {
		t.Fatalf("ConfigureInstance: %+v, %v", configured, err)
	}
	validated, err := svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{
		PluginInstanceID: testInstance,
		Bindings:         bindings,
	})
	if err != nil || !validated.Valid {
		t.Fatalf("ValidateBinding: %+v, %v", validated, err)
	}

	reader := newFakeEventReader()
	writer := newFakeEffectWriter()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	h := &harness{t: t, svc: svc, reader: reader, writer: writer, ctx: ctx, cancel: cancel, done: done}
	go func() { done <- svc.HandleEvents(ctx, reader, writer) }()
	waitForDefaultWriter(t, svc)
	t.Cleanup(h.close)
	return h
}

func (h *harness) close() {
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			h.t.Errorf("HandleEvents returned %v", err)
		}
	case <-time.After(2 * time.Second):
		h.t.Error("HandleEvents did not stop")
	}
}

func waitForDefaultWriter(t *testing.T, svc *Service) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		svc.mu.Lock()
		ready := svc.defaultWriter != nil
		svc.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("event stream did not register an effect writer")
}

func (h *harness) run(jobID, args, idem string) (*application.RunJobResponse, error) {
	return h.svc.RunJob(h.ctx, &application.RunJobRequest{
		PluginInstanceID: testInstance,
		JobID:            jobID,
		ArgsJSON:         args,
		IdempotencyKey:   idem,
	})
}

func (h *harness) send(union application.ApplicationEventUnion) {
	h.seq++
	h.reader.send(&application.ApplicationEvent{
		PluginInstanceID: testInstance,
		Sequence:         h.seq,
		SchemaVersion:    application.SchemaVersion,
		Union:            union,
	})
}

// deliver handles an event synchronously so state-machine tests do not depend
// on scheduling of the background event loop.
func (h *harness) deliver(union application.ApplicationEventUnion) error {
	return h.svc.handleEvent(&application.ApplicationEvent{
		PluginInstanceID: testInstance,
		SchemaVersion:    application.SchemaVersion,
		Union:            union,
	})
}

func (h *harness) commandKeys() []string {
	h.t.Helper()
	h.svc.mu.Lock()
	defer h.svc.mu.Unlock()
	st := h.svc.instances[testInstance]
	if st == nil || st.session == nil {
		h.t.Fatal("music session is missing")
	}
	return append([]string(nil), st.session.CommandOrder...)
}

func lastSessionRecord(t *testing.T, effects []*application.ApplicationEffect) map[string]any {
	t.Helper()
	for index := len(effects) - 1; index >= 0; index-- {
		upsert, ok := effects[index].Union.(*application.UpsertDomainRecord)
		if !ok || upsert.RecordType != sessionRecordType {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(upsert.DataJSON), &data); err != nil {
			t.Fatalf("decode session record: %v", err)
		}
		return data
	}
	t.Fatal("no music_session record in effects")
	return nil
}

func requestCommands(effects []*application.ApplicationEffect) []*application.RequestCommand {
	var commands []*application.RequestCommand
	for _, effect := range effects {
		if command, ok := effect.Union.(*application.RequestCommand); ok {
			commands = append(commands, command)
		}
	}
	return commands
}

const musicPlayerUIJSON = `{"apiVersion":1,"navigation":{"title":"音乐播放器","icon":"music","order":50,"route":"music","visibility":"instance-enabled"},"pages":[{"id":"home","title":"音乐播放器","sections":[{"type":"status","source":"instance"},{"type":"metrics","source":"records","recordType":"music_session"},{"type":"actions","source":"manual-jobs"},{"type":"records","source":"records","recordType":"music_session","presentation":"timeline"}]}]}`

func TestDescriptorAndManifestIdentity(t *testing.T) {
	svc := New()
	desc, err := svc.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if desc.ApplicationID != pluginIDValue || desc.Version != pluginVersion || desc.DeclarativeOnly {
		t.Fatalf("unexpected descriptor identity: %+v", desc)
	}
	if len(desc.Requirements) != 3 {
		t.Fatalf("requirements = %+v", desc.Requirements)
	}
	want := map[string]struct {
		capability  string
		cardinality string
	}{
		soundRequirement:     {soundCapability, "one"},
		displayRequirement:   {displayCapability, "zero-or-one"},
		indicatorRequirement: {indicatorCap, "zero-or-one"},
	}
	for _, requirement := range desc.Requirements {
		expected, ok := want[requirement.ID]
		if !ok || requirement.Capability != expected.capability || requirement.Cardinality != expected.cardinality {
			t.Fatalf("unexpected requirement: %+v", requirement)
		}
	}
	if len(desc.Jobs) != 3 {
		t.Fatalf("jobs = %+v", desc.Jobs)
	}
	for _, job := range desc.Jobs {
		if !job.ManualOnly || job.Title == "" || !json.Valid([]byte(job.InputSchemaJSON)) {
			t.Fatalf("invalid job descriptor: %+v", job)
		}
	}

	manifest := readJSONFile(t, "plugin.yaml")
	if manifest["id"] != pluginIDValue || manifest["version"] != pluginVersion || manifest["entrypoint"] != "cloud-path-app-music-player" {
		t.Fatalf("manifest identity drift: %+v", manifest)
	}
	requirements := readJSONFile(t, "requirements.yaml")["requirements"].([]any)
	if len(requirements) != 3 {
		t.Fatalf("requirements mirror = %+v", requirements)
	}
	contributes, ok := manifest["contributes"].(map[string]any)
	if !ok {
		t.Fatalf("manifest contributes = %#v", manifest["contributes"])
	}
	applications, ok := contributes["applications"].([]any)
	if !ok || len(applications) != 1 {
		t.Fatalf("manifest applications = %#v", contributes["applications"])
	}
	application, ok := applications[0].(map[string]any)
	if !ok {
		t.Fatalf("manifest application = %#v", applications[0])
	}
	uiBytes, err := json.Marshal(application["ui"])
	if err != nil {
		t.Fatalf("marshal UI contribution: %v", err)
	}
	var gotUI, wantUI any
	if err := json.Unmarshal(uiBytes, &gotUI); err != nil {
		t.Fatalf("invalid UI contribution: %v", err)
	}
	if err := json.Unmarshal([]byte(musicPlayerUIJSON), &wantUI); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotUI, wantUI) {
		t.Fatalf("UI contribution drift: %#v", gotUI)
	}
}

func TestPlaySongQueuesOneSequenceCommandAndRecord(t *testing.T) {
	h := newHarness(t, testBindings)
	response, err := h.run(jobPlaySong, `{"song":"little-star","repeat":1}`, "song-1")
	if err != nil || !response.Status.IsOK() {
		t.Fatalf("RunJob: %+v, %v", response, err)
	}

	notes := songCatalog[songLittleStar]
	effects := h.writer.waitFor(t, 2)
	commands := requestCommands(effects)
	if len(commands) != 1 {
		t.Fatalf("sequence commands = %d, want exactly one in-flight command", len(commands))
	}
	command := commands[0]
	if command.Action != toneSequenceAction || command.EntityID != testBindings[0].EntityID || command.IdempotencyKey == "" {
		t.Fatalf("command = %+v", command)
	}
	var got playSequenceArgs
	if err := json.Unmarshal([]byte(command.ArgsJSON), &got); err != nil {
		t.Fatalf("decode command args: %v", err)
	}
	if len(got.Notes) != len(notes) || got.GapMS != 0 {
		t.Fatalf("sequence args = %+v, want %d notes", got, len(notes))
	}
	for index := range notes {
		if got.Notes[index] != notes[index] {
			t.Fatalf("sequence note %d = %+v, want %+v", index, got.Notes[index], notes[index])
		}
	}

	record := lastSessionRecord(t, effects)
	if record["song"] != songLittleStar || record["status"] != statusQueued || record["repeat"] != float64(1) || record["last_note"] != nil {
		t.Fatalf("queued record = %+v", record)
	}
	if record["total_notes"] != float64(len(notes)) || record["queued_at"] == "" || record["request_id"] == "" || record["sound_bound"] != true || record["degraded"] != true {
		t.Fatalf("record missing required state: %+v", record)
	}
}

func TestPlayNoteBoundsAndValidation(t *testing.T) {
	h := newHarness(t, testBindings)
	valid := []string{
		`{"frequency_hz":1,"duration_ms":10}`,
		`{"frequency_hz":4000,"duration_ms":1200}`,
	}
	for index, args := range valid {
		before := h.writer.count()
		response, err := h.run(jobPlayNote, args, "valid-note-"+string(rune('a'+index)))
		if err != nil || !response.Status.IsOK() {
			t.Fatalf("valid note %s: %+v, %v", args, response, err)
		}
		effects := h.writer.waitFor(t, before+2)
		commands := requestCommands(effects)
		if len(commands) == 0 || commands[len(commands)-1].Action != toneAction {
			t.Fatalf("valid note did not emit tone: %+v", commands)
		}
		beforeCompletion := h.writer.count()
		h.send(&application.RequestCompleted{
			RequestID: commands[len(commands)-1].IdempotencyKey,
			EntityID:  testBindings[0].EntityID,
			Action:    toneAction,
			State:     application.CommandStateSucceeded,
		})
		h.writer.waitFor(t, beforeCompletion+1)
	}

	invalid := []string{
		`{"frequency_hz":0,"duration_ms":10}`,
		`{"frequency_hz":4001,"duration_ms":10}`,
		`{"frequency_hz":440,"duration_ms":9}`,
		`{"frequency_hz":440,"duration_ms":11}`,
		`{"frequency_hz":440,"duration_ms":1201}`,
		`{"frequency_hz":440,"duration_ms":1205}`,
		`{"frequency_hz":440,"duration_ms":10,"extra":1}`,
	}
	for index, args := range invalid {
		before := h.writer.count()
		if _, err := h.run(jobPlayNote, args, "invalid-note-"+string(rune('a'+index))); err == nil {
			t.Fatalf("invalid note accepted: %s", args)
		}
		if got := h.writer.count(); got != before {
			t.Fatalf("invalid note emitted effects: %s", args)
		}
	}

	before := h.writer.count()
	if _, err := h.run(jobPlaySong, `{"song":"unknown","repeat":1}`, "unknown-song"); err == nil {
		t.Fatal("unknown song accepted")
	}
	if _, err := h.run(jobPlaySong, `{"song":"birthday","repeat":4}`, "bad-repeat"); err == nil {
		t.Fatal("out-of-range repeat accepted")
	}
	if got := h.writer.count(); got != before {
		t.Fatalf("rejected song jobs emitted effects: got %d, want %d", got, before)
	}
}

func TestRequestCompletedAdvancesAndCompletesSession(t *testing.T) {
	h := newHarness(t, testBindings)
	response, err := h.run(jobPlaySong, `{"song":"ode-to-joy","repeat":2}`, "complete-song")
	if err != nil || !response.Status.IsOK() {
		t.Fatalf("RunJob: %+v, %v", response, err)
	}
	notes := songCatalog[songOdeToJoy]
	effects := h.writer.waitFor(t, 2)
	commands := requestCommands(effects)
	if len(commands) != 1 {
		t.Fatalf("initial commands = %d, want 1", len(commands))
	}
	if got := h.commandKeys(); len(got) != 2 {
		t.Fatalf("command order = %v, want one command per repetition", got)
	}

	first := commands[0]
	var firstArgs playSequenceArgs
	if err := json.Unmarshal([]byte(first.ArgsJSON), &firstArgs); err != nil {
		t.Fatalf("decode first sequence: %v", err)
	}
	if len(firstArgs.Notes) != len(notes) {
		t.Fatalf("first sequence notes = %d, want %d", len(firstArgs.Notes), len(notes))
	}

	before := h.writer.count()
	h.send(&application.RequestCompleted{
		RequestID: first.IdempotencyKey,
		EntityID:  testBindings[0].EntityID,
		Action:    toneSequenceAction,
		State:     application.CommandStateSucceeded,
	})
	effects = h.writer.waitFor(t, before+2)
	record := lastSessionRecord(t, effects)
	if record["status"] != statusPlaying || record["completed_notes"] != float64(len(notes)) {
		t.Fatalf("after first repetition record = %+v", record)
	}
	lastNote, ok := record["last_note"].(map[string]any)
	if !ok || lastNote["index"] != float64(len(notes)) {
		t.Fatalf("last_note after first repetition = %+v", record["last_note"])
	}
	commands = requestCommands(effects)
	second := commands[len(commands)-1]
	if second.IdempotencyKey == first.IdempotencyKey {
		t.Fatal("completion did not advance to the next repetition")
	}

	before = h.writer.count()
	h.send(&application.RequestCompleted{
		RequestID: second.IdempotencyKey,
		EntityID:  testBindings[0].EntityID,
		Action:    toneSequenceAction,
		State:     application.CommandStateSucceeded,
	})
	effects = h.writer.waitFor(t, before+1)
	record = lastSessionRecord(t, effects)
	if record["status"] != statusCompleted || record["completed_notes"] != float64(len(notes)*2) {
		t.Fatalf("completed record = %+v", record)
	}
	lastNote, ok = record["last_note"].(map[string]any)
	if !ok || lastNote["index"] != float64(len(notes)*2) {
		t.Fatalf("final last_note = %+v", record["last_note"])
	}

	before = h.writer.count()
	h.send(&application.RequestCompleted{
		RequestID: second.IdempotencyKey,
		EntityID:  testBindings[0].EntityID,
		Action:    toneSequenceAction,
		State:     application.CommandStateSucceeded,
	})
	time.Sleep(20 * time.Millisecond)
	if got := h.writer.count(); got != before {
		t.Fatalf("duplicate completion emitted effects: got %d, want %d", got, before)
	}
}

func TestRequestCompletedFailureAndNonTerminalStates(t *testing.T) {
	h := newHarness(t, testBindings)
	response, err := h.run(jobPlayNote, `{"frequency_hz":440,"duration_ms":100}`, "failure-song")
	if err != nil || !response.Status.IsOK() {
		t.Fatalf("RunJob: %+v, %v", response, err)
	}
	effects := h.writer.waitFor(t, 2)
	command := requestCommands(effects)[0]

	before := h.writer.count()
	h.send(&application.RequestCompleted{
		RequestID: command.IdempotencyKey,
		EntityID:  testBindings[0].EntityID,
		Action:    toneAction,
		State:     application.CommandStateRunning,
	})
	time.Sleep(20 * time.Millisecond)
	if got := h.writer.count(); got != before {
		t.Fatalf("non-terminal state emitted effects: got %d, want %d", got, before)
	}

	h.send(&application.RequestCompleted{
		RequestID:  command.IdempotencyKey,
		EntityID:   testBindings[0].EntityID,
		Action:     toneAction,
		State:      application.CommandStateFailed,
		ErrorCode:  "DEVICE_REJECTED",
		ResultJSON: `{"detail":"badarg"}`,
	})
	effects = h.writer.waitFor(t, before+1)
	record := lastSessionRecord(t, effects)
	if record["status"] != statusFailed || record["error_code"] != "DEVICE_REJECTED" || record["failed_note"] == nil {
		t.Fatalf("failed record = %+v", record)
	}
}

func TestToneFailureDoesNotQueueNextRepetition(t *testing.T) {
	h := newHarness(t, testBindings)
	response, err := h.run(jobPlaySong, `{"song":"little-star","repeat":3}`, "stop-on-failure")
	if err != nil || !response.Status.IsOK() {
		t.Fatalf("RunJob: %+v, %v", response, err)
	}
	effects := h.writer.waitFor(t, 2)
	command := requestCommands(effects)[0]
	before := h.writer.count()
	h.send(&application.RequestCompleted{
		RequestID:  command.IdempotencyKey,
		EntityID:   testBindings[0].EntityID,
		Action:     toneSequenceAction,
		State:      application.CommandStateFailed,
		ErrorCode:  "DEVICE_REJECTED",
		ResultJSON: `{"detail":"badarg"}`,
	})
	effects = h.writer.waitFor(t, before+1)
	record := lastSessionRecord(t, effects)
	if record["status"] != statusFailed || record["failed_note"] == nil {
		t.Fatalf("failed record = %+v", record)
	}
	time.Sleep(20 * time.Millisecond)
	if got := h.writer.count(); got != before+1 {
		t.Fatalf("failure queued a subsequent repetition: effects=%d, want %d", got, before+1)
	}
}

func TestTerminalRequestCompletedFailsSessionAndStopsSequence(t *testing.T) {
	tests := []struct {
		name      string
		state     application.CommandState
		errorCode string
	}{
		{name: "failed", state: application.CommandStateFailed, errorCode: "DEVICE_REJECTED"},
		{name: "timed_out", state: application.CommandStateTimedOut, errorCode: "COMMAND_TIMEOUT"},
		{name: "cancelled", state: application.CommandStateCancelled, errorCode: "COMMAND_CANCELLED"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, testBindings)
			response, err := h.run(jobPlaySong, `{"song":"little-star","repeat":3}`, "terminal-"+test.name)
			if err != nil || !response.Status.IsOK() {
				t.Fatalf("RunJob: %+v, %v", response, err)
			}

			effects := h.writer.waitFor(t, 2)
			commands := requestCommands(effects)
			if len(commands) != 1 {
				t.Fatalf("initial sequence commands = %d, want 1", len(commands))
			}
			keys := h.commandKeys()
			if len(keys) != 3 {
				t.Fatalf("command order = %v, want one command per repetition", keys)
			}

			first := commands[0]
			before := h.writer.count()
			if err := h.deliver(&application.RequestCompleted{
				RequestID:  first.IdempotencyKey,
				EntityID:   testBindings[0].EntityID,
				Action:     toneSequenceAction,
				State:      test.state,
				ErrorCode:  test.errorCode,
				ResultJSON: `{"detail":"terminal"}`,
			}); err != nil {
				t.Fatalf("deliver terminal event: %v", err)
			}
			effects = h.writer.waitFor(t, before+1)
			if got := len(effects) - before; got != 1 {
				t.Fatalf("terminal event emitted %d effects, want only the session record", got)
			}
			if got := len(requestCommands(effects[before:])); got != 0 {
				t.Fatalf("terminal event emitted %d next commands, want 0", got)
			}

			record := lastSessionRecord(t, effects[before:])
			if record["status"] != statusFailed || record["completed_notes"] != float64(0) {
				t.Fatalf("terminal record = %+v", record)
			}
			if record["error_code"] != test.errorCode || record["failed_note"] == nil {
				t.Fatalf("terminal failure details = %+v", record)
			}

			if err := h.deliver(&application.RequestCompleted{
				RequestID:  first.IdempotencyKey,
				EntityID:   testBindings[0].EntityID,
				Action:     toneSequenceAction,
				State:      test.state,
				ErrorCode:  test.errorCode,
				ResultJSON: `{"detail":"duplicate"}`,
			}); err != nil {
				t.Fatalf("deliver duplicate terminal event: %v", err)
			}
			if err := h.deliver(&application.RequestCompleted{
				RequestID: keys[1],
				EntityID:  testBindings[0].EntityID,
				Action:    toneSequenceAction,
				State:     application.CommandStateSucceeded,
			}); err != nil {
				t.Fatalf("deliver future success: %v", err)
			}
			if got := h.writer.count(); got != before+1 {
				t.Fatalf("events after terminal state emitted effects: got %d, want %d", got, before+1)
			}
		})
	}
}

func TestRequestCompletedIgnoresOutOfOrderAndDuplicateEvents(t *testing.T) {
	h := newHarness(t, testBindings)
	response, err := h.run(jobPlaySong, `{"song":"little-star","repeat":3}`, "ordered-song")
	if err != nil || !response.Status.IsOK() {
		t.Fatalf("RunJob: %+v, %v", response, err)
	}

	effects := h.writer.waitFor(t, 2)
	commands := requestCommands(effects)
	if len(commands) != 1 {
		t.Fatalf("initial sequence commands = %d, want 1", len(commands))
	}
	keys := h.commandKeys()
	if len(keys) != 3 {
		t.Fatalf("command order = %v, want one command per repetition", keys)
	}

	first := commands[0]
	before := h.writer.count()
	for _, key := range []string{keys[1], keys[2]} {
		if err := h.deliver(&application.RequestCompleted{
			RequestID: key,
			EntityID:  testBindings[0].EntityID,
			Action:    toneSequenceAction,
			State:     application.CommandStateSucceeded,
		}); err != nil {
			t.Fatalf("deliver out-of-order success: %v", err)
		}
		if got := h.writer.count(); got != before {
			t.Fatalf("out-of-order success emitted effects: got %d, want %d", got, before)
		}
	}

	if err := h.deliver(&application.RequestCompleted{
		RequestID: first.IdempotencyKey,
		EntityID:  testBindings[0].EntityID,
		Action:    toneSequenceAction,
		State:     application.CommandStateSucceeded,
	}); err != nil {
		t.Fatalf("deliver first success: %v", err)
	}
	effects = h.writer.waitFor(t, before+2)
	commands = requestCommands(effects)
	second := commands[len(commands)-1]
	if second.IdempotencyKey != keys[1] {
		t.Fatalf("next command = %q, want %q", second.IdempotencyKey, keys[1])
	}

	before = h.writer.count()
	for _, key := range []string{keys[0], keys[2]} {
		if err := h.deliver(&application.RequestCompleted{
			RequestID: key,
			EntityID:  testBindings[0].EntityID,
			Action:    toneSequenceAction,
			State:     application.CommandStateSucceeded,
		}); err != nil {
			t.Fatalf("deliver duplicate/out-of-order success: %v", err)
		}
		if got := h.writer.count(); got != before {
			t.Fatalf("duplicate/out-of-order success emitted effects: got %d, want %d", got, before)
		}
	}

	if err := h.deliver(&application.RequestCompleted{
		RequestID:  second.IdempotencyKey,
		EntityID:   testBindings[0].EntityID,
		Action:     toneSequenceAction,
		State:      application.CommandStateTimedOut,
		ErrorCode:  "COMMAND_TIMEOUT",
		ResultJSON: `{"detail":"timeout"}`,
	}); err != nil {
		t.Fatalf("deliver second timeout: %v", err)
	}
	effects = h.writer.waitFor(t, before+1)
	record := lastSessionRecord(t, effects[before:])
	if record["status"] != statusFailed || record["completed_notes"] != float64(len(songCatalog[songLittleStar])) {
		t.Fatalf("timeout record = %+v", record)
	}

	before = h.writer.count()
	for _, event := range []*application.RequestCompleted{
		{RequestID: second.IdempotencyKey, EntityID: testBindings[0].EntityID, Action: toneSequenceAction, State: application.CommandStateTimedOut},
		{RequestID: keys[2], EntityID: testBindings[0].EntityID, Action: toneSequenceAction, State: application.CommandStateSucceeded},
	} {
		if err := h.deliver(event); err != nil {
			t.Fatalf("deliver replay after failure: %v", err)
		}
	}
	if got := h.writer.count(); got != before {
		t.Fatalf("replay after failure emitted effects: got %d, want %d", got, before)
	}
}

func TestIdempotencyKeyIsStableAndRejectsDrift(t *testing.T) {
	h := newHarness(t, testBindings)
	args := `{"song":"birthday","repeat":2}`
	first, err := h.run(jobPlaySong, args, "same-key")
	if err != nil || !first.Status.IsOK() {
		t.Fatalf("first run: %+v, %v", first, err)
	}
	firstCount := h.writer.count()
	firstRecord := lastSessionRecord(t, h.writer.snapshot())
	firstRequestID := firstRecord["request_id"]

	second, err := h.run(jobPlaySong, args, "same-key")
	if err != nil || !second.Status.IsOK() {
		t.Fatalf("second run: %+v, %v", second, err)
	}
	if second.ResultJSON != first.ResultJSON || h.writer.count() != firstCount {
		t.Fatal("idempotent retry emitted duplicate effects or changed result")
	}
	secondRecord := readJSON(t, second.ResultJSON)
	if secondRecord["request_id"] != firstRequestID {
		t.Fatalf("request_id changed on retry: %v -> %v", firstRequestID, secondRecord["request_id"])
	}

	if _, err := h.run(jobPlaySong, `{"song":"birthday","repeat":1}`, "same-key"); err == nil {
		t.Fatal("idempotency key reuse with different args was accepted")
	} else {
		var st *status.Status
		if !errors.As(err, &st) || st.Code != status.CodeInvalidArgument {
			t.Fatalf("drift error = %v, want INVALID_ARGUMENT", err)
		}
	}
}

func TestBindingDegradationAndStatus(t *testing.T) {
	h := newHarness(t, testBindings)
	response, err := h.run(jobStatus, `{}`, "status-1")
	if err != nil || !response.Status.IsOK() {
		t.Fatalf("status: %+v, %v", response, err)
	}
	record := readJSON(t, response.ResultJSON)
	if record["status"] != statusIdle || record["degraded"] != true || record["local_display_bound"] != false || record["indicator_bound"] != false {
		t.Fatalf("degraded status = %+v", record)
	}

	validated, err := h.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{
		PluginInstanceID: testInstance,
		Bindings: []application.Binding{
			{RequirementID: soundRequirement, EntityID: "entity/buzzer"},
			{RequirementID: displayRequirement, EntityID: "entity/display"},
			{RequirementID: indicatorRequirement, EntityID: "entity/led"},
		},
	})
	if err != nil || !validated.Valid {
		t.Fatalf("optional bindings: %+v, %v", validated, err)
	}
	response, err = h.run(jobStatus, `{}`, "status-2")
	if err != nil || !response.Status.IsOK() {
		t.Fatalf("status after optional bindings: %+v, %v", response, err)
	}
	record = readJSON(t, response.ResultJSON)
	if record["degraded"] != false || record["local_display_bound"] != true || record["indicator_bound"] != true {
		t.Fatalf("non-degraded status = %+v", record)
	}

	invalid := []application.Binding{
		{RequirementID: soundRequirement, EntityID: "entity/buzzer"},
		{RequirementID: soundRequirement, EntityID: "entity/buzzer-2"},
	}
	if result, err := h.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{PluginInstanceID: testInstance, Bindings: invalid}); err != nil || result.Valid {
		t.Fatalf("duplicate sound binding accepted: %+v, %v", result, err)
	}
	unknown := []application.Binding{{RequirementID: "driver:vendor", EntityID: "entity/driver"}}
	if result, err := h.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{PluginInstanceID: testInstance, Bindings: unknown}); err != nil || result.Valid {
		t.Fatalf("unknown requirement accepted: %+v, %v", result, err)
	}
}

func TestConfigureRejectsUnknownFieldsAndHTTPStatus(t *testing.T) {
	svc := New()
	response, err := svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{
		PluginInstanceID: testInstance,
		Config:           []byte(`{"unexpected":true}`),
		ConfigRevision:   1,
	})
	if err != nil || response.Status.IsOK() {
		t.Fatalf("unknown config accepted: %+v, %v", response, err)
	}

	h := newHarness(t, testBindings)
	httpResponse, err := h.svc.HandleRequest(context.Background(), &application.PluginHTTPRequest{
		Method:           "GET",
		Path:             "/status",
		PluginInstanceID: testInstance,
		Context:          application.RequestContext{InstanceID: testInstance},
	})
	if err != nil || httpResponse.StatusCode != 200 || !strings.Contains(string(httpResponse.Body), `"status":"idle"`) {
		t.Fatalf("GET /status: %+v, %v", httpResponse, err)
	}
	httpResponse, err = h.svc.HandleRequest(context.Background(), &application.PluginHTTPRequest{
		Method:           "POST",
		Path:             "/status",
		PluginInstanceID: testInstance,
		Context:          application.RequestContext{InstanceID: testInstance},
	})
	if err != nil || httpResponse.StatusCode != 405 {
		t.Fatalf("POST /status: %+v, %v", httpResponse, err)
	}
}

func TestSongCatalogNotesSatisfyToneContract(t *testing.T) {
	for song, notes := range songCatalog {
		for index, note := range notes {
			if err := validateNote(note); err != nil {
				t.Fatalf("%s note %d violates tone contract: %+v: %v", song, index, note, err)
			}
		}
	}
}

func TestSingleActiveSessionAndRebindingGuard(t *testing.T) {
	h := newHarness(t, testBindings)
	first, err := h.run(jobPlaySong, `{"song":"little-star","repeat":1}`, "active-1")
	if err != nil || !first.Status.IsOK() {
		t.Fatalf("first session: %+v, %v", first, err)
	}
	before := h.writer.count()

	if _, err := h.run(jobPlayNote, `{"frequency_hz":440,"duration_ms":100}`, "active-2"); err == nil {
		t.Fatal("overlapping play session was accepted")
	}
	if got := h.writer.count(); got != before {
		t.Fatalf("overlapping session emitted effects: got %d, want %d", got, before)
	}

	changed, err := h.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{
		PluginInstanceID: testInstance,
		Bindings:         []application.Binding{{RequirementID: soundRequirement, EntityID: "entity/other-buzzer"}},
	})
	if err != nil || changed.Valid {
		t.Fatalf("rebinding while active was accepted: %+v, %v", changed, err)
	}
	unchanged, err := h.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{
		PluginInstanceID: testInstance,
		Bindings:         testBindings,
	})
	if err != nil || !unchanged.Valid {
		t.Fatalf("same binding while active was rejected: %+v, %v", unchanged, err)
	}
}
func readJSONFile(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return readJSON(t, string(data))
}

func readJSON(t *testing.T, text string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		t.Fatalf("decode JSON %q: %v", text, err)
	}
	return value
}
