// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Cloudpath Authors

package plugin

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestToneSequenceContractAndNativeMapping(t *testing.T) {
	args, err := parseToneSequenceArgs(`{"notes":[{"frequency_hz":262,"duration_ms":300},{"frequency_hz":392,"duration_ms":600}],"gap_ms":10}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(args.Notes) != 2 || args.GapMS != 10 {
		t.Fatalf("args = %+v", args)
	}
	if _, err := parseToneSequenceArgs(`{"notes":[{"frequency_hz":262,"duration_ms":300}],"gap_ms":0,"extra":1}`); err == nil {
		t.Fatal("tone-sequence accepted unknown field")
	}
	if _, err := parseToneSequenceArgs(`{"notes":[]}`); err == nil {
		t.Fatal("tone-sequence accepted empty notes")
	}
	if _, err := parseToneSequenceArgs(`{"notes":[{"frequency_hz":0,"duration_ms":10}]}`); err == nil {
		t.Fatal("tone-sequence accepted invalid frequency")
	}
	if _, err := parseToneSequenceArgs(`{"notes":[{"frequency_hz":262,"duration_ms":11}]}`); err == nil {
		t.Fatal("tone-sequence accepted non-10ms duration")
	}
	if _, err := parseToneSequenceArgs(`{"notes":[{"frequency_hz":262,"duration_ms":1200}],"gap_ms":1001}`); err == nil {
		t.Fatal("tone-sequence accepted oversized gap")
	}
	notes := make([]toneNote, 60)
	for i := range notes {
		notes[i] = toneNote{FrequencyHz: 1000, DurationMS: 1000}
	}
	raw, err := json.Marshal(toneSequenceArgs{Notes: notes, GapMS: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseToneSequenceArgs(string(raw)); err == nil {
		t.Fatal("tone-sequence ignored gap_ms in total duration")
	}

	littleStar := toneSequenceArgs{Notes: []toneNote{
		{FrequencyHz: 262, DurationMS: 300}, {FrequencyHz: 262, DurationMS: 300},
		{FrequencyHz: 392, DurationMS: 300}, {FrequencyHz: 392, DurationMS: 300},
		{FrequencyHz: 440, DurationMS: 300}, {FrequencyHz: 440, DurationMS: 300},
		{FrequencyHz: 392, DurationMS: 600}, {FrequencyHz: 349, DurationMS: 300},
		{FrequencyHz: 349, DurationMS: 300}, {FrequencyHz: 330, DurationMS: 300},
		{FrequencyHz: 330, DurationMS: 300}, {FrequencyHz: 294, DurationMS: 300},
		{FrequencyHz: 294, DurationMS: 300}, {FrequencyHz: 262, DurationMS: 600},
	}}
	if song, ok := nativeSongForSequence(littleStar); !ok || song != "little-star" {
		t.Fatalf("native mapping = %q, %v", song, ok)
	}
	littleStar.GapMS = 10
	if _, ok := nativeSongForSequence(littleStar); ok {
		t.Fatal("native mapping ignored gap_ms")
	}
}

func TestEncodeNativeSongAndActionDeclaration(t *testing.T) {
	frame, err := encodeV1Command("c1", actionSong, `{"song":"little-star"}`)
	if err != nil || string(frame) != "CMD:c1:song:name=little-star\r\n" {
		t.Fatalf("song frame = %q err=%v", frame, err)
	}
	if containsAction(supportedActions, actionSong) {
		t.Fatal("internal song verb must not be exposed as an action")
	}
	if !containsAction(supportedActions, actionToneSequence) {
		t.Fatal("tone-sequence action is not exposed")
	}
	if !slowActions[actionToneSequence] {
		t.Fatal("tone-sequence is not marked as a slow device action")
	}
	var found bool
	for _, capability := range capabilityDescriptors() {
		if capability.ID != capBuzzer {
			continue
		}
		for _, action := range capability.Actions {
			if action.Name != actionToneSequence {
				continue
			}
			found = true
			if strings.TrimSpace(action.Title) == "" || strings.TrimSpace(action.Description) == "" {
				t.Fatal("tone-sequence descriptor is missing presentation metadata")
			}
			var schema map[string]any
			if err := json.Unmarshal([]byte(action.InputSchemaJSON), &schema); err != nil {
				t.Fatalf("tone-sequence schema: %v", err)
			}
			if schema["additionalProperties"] != false {
				t.Fatal("tone-sequence schema must reject additional properties")
			}
		}
	}
	if !found {
		t.Fatal("buzzer capability does not declare tone-sequence")
	}
}

func TestToneSequenceNativeFastPath(t *testing.T) {
	p := &fakePort{}
	d := newFakeV1Device("board-native", p)
	args := `{"notes":[{"frequency_hz":262,"duration_ms":300},{"frequency_hz":262,"duration_ms":300},{"frequency_hz":392,"duration_ms":300},{"frequency_hz":392,"duration_ms":300},{"frequency_hz":440,"duration_ms":300},{"frequency_hz":440,"duration_ms":300},{"frequency_hz":392,"duration_ms":600},{"frequency_hz":349,"duration_ms":300},{"frequency_hz":349,"duration_ms":300},{"frequency_hz":330,"duration_ms":300},{"frequency_hz":330,"duration_ms":300},{"frequency_hz":294,"duration_ms":300},{"frequency_hz":294,"duration_ms":300},{"frequency_hz":262,"duration_ms":600}]}`
	result := make(chan error, 1)
	go func() {
		_, err := d.sendV1ToneSequence(context.Background(), "seq-native", args)
		result <- err
	}()
	waitForWrite(t, p, "CMD:seq-native:song:name=little-star")
	d.handleLine("ACK:seq-native:ok")
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("native sequence did not complete")
	}
	if strings.Contains(p.joined(), ":beep:") {
		t.Fatalf("native fast path unexpectedly fell back to per-note beep: %q", p.joined())
	}
}

func TestToneSequenceFallsBackAfterUnknownNativeSong(t *testing.T) {
	p := &fakePort{}
	d := newFakeV1Device("board-old", p)
	args := `{"notes":[{"frequency_hz":262,"duration_ms":300},{"frequency_hz":262,"duration_ms":300},{"frequency_hz":392,"duration_ms":300},{"frequency_hz":392,"duration_ms":300},{"frequency_hz":440,"duration_ms":300},{"frequency_hz":440,"duration_ms":300},{"frequency_hz":392,"duration_ms":600},{"frequency_hz":349,"duration_ms":300},{"frequency_hz":349,"duration_ms":300},{"frequency_hz":330,"duration_ms":300},{"frequency_hz":330,"duration_ms":300},{"frequency_hz":294,"duration_ms":300},{"frequency_hz":294,"duration_ms":300},{"frequency_hz":262,"duration_ms":600}]}`
	result := make(chan error, 1)
	go func() {
		_, err := d.sendV1ToneSequence(context.Background(), "seq-old", args)
		result <- err
	}()
	waitForWrite(t, p, "CMD:seq-old:song:name=little-star")
	d.handleLine("ERR:seq-old:unknown")
	for index := 1; index <= 14; index++ {
		id := "seq-old-" + strconv.Itoa(index)
		waitForWrite(t, p, "CMD:"+id+":beep")
		d.handleLine("ACK:" + id + ":ok")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fallback sequence did not complete")
	}
}

func waitForWrite(t *testing.T, p *fakePort, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(p.joined(), want) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for UART write %q; got %q", want, p.joined())
}
