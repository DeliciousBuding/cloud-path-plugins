// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Cloudpath Authors

package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	maxToneSequenceNotes = 64
	maxToneSequenceMS    = 60000
	maxSequenceGapMS     = 1000
	defaultV1ACKTimeout  = 4 * time.Second
	nativeSongACKTimeout = 20 * time.Second
)

type toneNote struct {
	FrequencyHz int `json:"frequency_hz"`
	DurationMS  int `json:"duration_ms"`
}

type toneSequenceArgs struct {
	Notes []toneNote `json:"notes"`
	GapMS int        `json:"gap_ms,omitempty"`
}

type nativeSongArgs struct {
	Song string `json:"song"`
}

func decodeStrictObject(raw string, dst any) error {
	text := strings.TrimSpace(raw)
	if text == "" {
		return fmt.Errorf("args must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("args must contain exactly one JSON object")
	}
	return nil
}

func validateToneNote(note toneNote) error {
	if note.FrequencyHz < 1 || note.FrequencyHz > 4000 {
		return fmt.Errorf("frequency_hz must be an integer from 1 to 4000")
	}
	if note.DurationMS < 10 || note.DurationMS > 1200 || note.DurationMS%10 != 0 {
		return fmt.Errorf("duration_ms must be an integer from 10 to 1200 and a multiple of 10")
	}
	return nil
}

func parseToneSequenceArgs(raw string) (toneSequenceArgs, error) {
	var args toneSequenceArgs
	if err := decodeStrictObject(raw, &args); err != nil {
		return toneSequenceArgs{}, fmt.Errorf("stcb: tone-sequence args: %w", err)
	}
	if len(args.Notes) == 0 || len(args.Notes) > maxToneSequenceNotes {
		return toneSequenceArgs{}, fmt.Errorf("stcb: tone-sequence requires 1-%d notes", maxToneSequenceNotes)
	}
	if args.GapMS < 0 || args.GapMS > maxSequenceGapMS || args.GapMS%10 != 0 {
		return toneSequenceArgs{}, fmt.Errorf("stcb: tone-sequence gap_ms must be 0-%d and a multiple of 10", maxSequenceGapMS)
	}
	total := 0
	for index, note := range args.Notes {
		if err := validateToneNote(note); err != nil {
			return toneSequenceArgs{}, fmt.Errorf("stcb: tone-sequence note %d: %w", index+1, err)
		}
		total += note.DurationMS
		if index < len(args.Notes)-1 {
			total += args.GapMS
		}
	}
	if total > maxToneSequenceMS {
		return toneSequenceArgs{}, fmt.Errorf("stcb: tone-sequence total duration exceeds %d ms", maxToneSequenceMS)
	}
	return args, nil
}

func parseNativeSongArgs(raw string) (nativeSongArgs, error) {
	var args nativeSongArgs
	if err := decodeStrictObject(raw, &args); err != nil {
		return nativeSongArgs{}, fmt.Errorf("stcb: song args: %w", err)
	}
	switch args.Song {
	case "little-star", "birthday", "ode-to-joy":
		return args, nil
	default:
		return nativeSongArgs{}, fmt.Errorf("stcb: song must be little-star, birthday or ode-to-joy")
	}
}

func sequenceKey(notes []toneNote) string {
	var b strings.Builder
	for index, note := range notes {
		if index > 0 {
			b.WriteByte(';')
		}
		fmt.Fprintf(&b, "%d,%d", note.FrequencyHz, note.DurationMS)
	}
	return b.String()
}

var nativeSongBySequence = func() map[string]string {
	return map[string]string{
		sequenceKey([]toneNote{
			{FrequencyHz: 262, DurationMS: 300}, {FrequencyHz: 262, DurationMS: 300},
			{FrequencyHz: 392, DurationMS: 300}, {FrequencyHz: 392, DurationMS: 300},
			{FrequencyHz: 440, DurationMS: 300}, {FrequencyHz: 440, DurationMS: 300},
			{FrequencyHz: 392, DurationMS: 600}, {FrequencyHz: 349, DurationMS: 300},
			{FrequencyHz: 349, DurationMS: 300}, {FrequencyHz: 330, DurationMS: 300},
			{FrequencyHz: 330, DurationMS: 300}, {FrequencyHz: 294, DurationMS: 300},
			{FrequencyHz: 294, DurationMS: 300}, {FrequencyHz: 262, DurationMS: 600},
		}): "little-star",
		sequenceKey([]toneNote{
			{FrequencyHz: 392, DurationMS: 250}, {FrequencyHz: 392, DurationMS: 250},
			{FrequencyHz: 440, DurationMS: 500}, {FrequencyHz: 392, DurationMS: 500},
			{FrequencyHz: 523, DurationMS: 500}, {FrequencyHz: 494, DurationMS: 750},
			{FrequencyHz: 392, DurationMS: 250}, {FrequencyHz: 392, DurationMS: 250},
			{FrequencyHz: 440, DurationMS: 500}, {FrequencyHz: 392, DurationMS: 500},
			{FrequencyHz: 587, DurationMS: 500}, {FrequencyHz: 523, DurationMS: 750},
			{FrequencyHz: 392, DurationMS: 250}, {FrequencyHz: 392, DurationMS: 250},
			{FrequencyHz: 784, DurationMS: 500}, {FrequencyHz: 659, DurationMS: 500},
			{FrequencyHz: 523, DurationMS: 500}, {FrequencyHz: 494, DurationMS: 500},
			{FrequencyHz: 440, DurationMS: 750}, {FrequencyHz: 698, DurationMS: 250},
			{FrequencyHz: 698, DurationMS: 250}, {FrequencyHz: 659, DurationMS: 500},
			{FrequencyHz: 523, DurationMS: 500}, {FrequencyHz: 587, DurationMS: 500},
			{FrequencyHz: 523, DurationMS: 750},
		}): "birthday",
		sequenceKey([]toneNote{
			{FrequencyHz: 330, DurationMS: 250}, {FrequencyHz: 330, DurationMS: 250},
			{FrequencyHz: 349, DurationMS: 250}, {FrequencyHz: 392, DurationMS: 250},
			{FrequencyHz: 392, DurationMS: 250}, {FrequencyHz: 349, DurationMS: 250},
			{FrequencyHz: 330, DurationMS: 250}, {FrequencyHz: 294, DurationMS: 250},
			{FrequencyHz: 262, DurationMS: 250}, {FrequencyHz: 262, DurationMS: 250},
			{FrequencyHz: 294, DurationMS: 250}, {FrequencyHz: 330, DurationMS: 250},
			{FrequencyHz: 330, DurationMS: 380}, {FrequencyHz: 294, DurationMS: 120},
			{FrequencyHz: 294, DurationMS: 500},
		}): "ode-to-joy",
	}
}()

func nativeSongForSequence(args toneSequenceArgs) (string, bool) {
	if args.GapMS != 0 {
		return "", false
	}
	song, ok := nativeSongBySequence[sequenceKey(args.Notes)]
	return song, ok
}

func isUnknownActionError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "code=unknown")
}

func waitSequenceGap(ctx context.Context, d *device, gap time.Duration) error {
	timer := time.NewTimer(gap)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.done:
		return fmt.Errorf("stcb: port dead during sequence gap")
	case <-timer.C:
		return nil
	}
}

// sendV1ToneSequence keeps the sequence contract device-agnostic while giving
// STC-B a firmware-native fast path for its built-in songs. Unknown or
// non-matching sequences fall back to one correlated beep command per note.
func (d *device) sendV1ToneSequence(ctx context.Context, id, argsJSON string) (string, error) {
	args, err := parseToneSequenceArgs(argsJSON)
	if err != nil {
		return "", err
	}
	d.cmdMu.Lock()
	defer d.cmdMu.Unlock()

	if song, ok := nativeSongForSequence(args); ok {
		nativeArgs := mustJSON(nativeSongArgs{Song: song})
		detail, nativeErr := d.sendV1CommandLocked(ctx, id, actionSong, nativeArgs, nativeSongACKTimeout)
		if nativeErr == nil {
			return fmt.Sprintf("%s native-song=%s notes=%d", detail, song, len(args.Notes)), nil
		}
		if !isUnknownActionError(nativeErr) {
			return "", nativeErr
		}
		// Older Protocol v1 firmware does not know the internal song verb.
		// Only an explicit unknown-action error is safe to fall back from;
		// timeout/busy/dead-port errors must stay visible and must not replay.
	}

	for index, note := range args.Notes {
		noteID := fmt.Sprintf("%s-%d", id, index+1)
		noteJSON := mustJSON(note)
		if _, err := d.sendV1CommandLocked(ctx, noteID, actionTone, noteJSON, defaultV1ACKTimeout); err != nil {
			return "", fmt.Errorf("tone-sequence note %d/%d: %w", index+1, len(args.Notes), err)
		}
		if args.GapMS > 0 && index+1 < len(args.Notes) {
			if err := waitSequenceGap(ctx, d, time.Duration(args.GapMS)*time.Millisecond); err != nil {
				return "", err
			}
		}
	}
	return fmt.Sprintf("device ACK sequence notes=%d native=fallback", len(args.Notes)), nil
}
