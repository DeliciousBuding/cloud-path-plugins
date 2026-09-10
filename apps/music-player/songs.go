// SPDX-License-Identifier: Apache-2.0

package musicplayer

import (
	"errors"
	"fmt"
)

const (
	songLittleStar = "little-star"
	songBirthday   = "birthday"
	songOdeToJoy   = "ode-to-joy"
)

// Note is the public tone payload. The buzzer capability contract is
// intentionally expressed in physical units, not driver-specific steps.
type Note struct {
	FrequencyHz int `json:"frequency_hz"`
	DurationMS  int `json:"duration_ms"`
}

// NoteResult identifies the last successfully completed note of a session.
type NoteResult struct {
	Index       int `json:"index"`
	FrequencyHz int `json:"frequency_hz"`
	DurationMS  int `json:"duration_ms"`
}

var songCatalog = map[string][]Note{
	songLittleStar: {
		{FrequencyHz: 262, DurationMS: 300}, // C4
		{FrequencyHz: 262, DurationMS: 300},
		{FrequencyHz: 392, DurationMS: 300}, // G4
		{FrequencyHz: 392, DurationMS: 300},
		{FrequencyHz: 440, DurationMS: 300}, // A4
		{FrequencyHz: 440, DurationMS: 300},
		{FrequencyHz: 392, DurationMS: 600},
		{FrequencyHz: 349, DurationMS: 300}, // F4
		{FrequencyHz: 349, DurationMS: 300},
		{FrequencyHz: 330, DurationMS: 300}, // E4
		{FrequencyHz: 330, DurationMS: 300},
		{FrequencyHz: 294, DurationMS: 300}, // D4
		{FrequencyHz: 294, DurationMS: 300},
		{FrequencyHz: 262, DurationMS: 600}, // C4
	},
	songBirthday: {
		{FrequencyHz: 392, DurationMS: 250}, // G4
		{FrequencyHz: 392, DurationMS: 250},
		{FrequencyHz: 440, DurationMS: 500}, // A4
		{FrequencyHz: 392, DurationMS: 500},
		{FrequencyHz: 523, DurationMS: 500}, // C5
		{FrequencyHz: 494, DurationMS: 750}, // B4
		{FrequencyHz: 392, DurationMS: 250},
		{FrequencyHz: 392, DurationMS: 250},
		{FrequencyHz: 440, DurationMS: 500},
		{FrequencyHz: 392, DurationMS: 500},
		{FrequencyHz: 587, DurationMS: 500}, // D5
		{FrequencyHz: 523, DurationMS: 750}, // C5
		{FrequencyHz: 392, DurationMS: 250},
		{FrequencyHz: 392, DurationMS: 250},
		{FrequencyHz: 784, DurationMS: 500}, // G5
		{FrequencyHz: 659, DurationMS: 500}, // E5
		{FrequencyHz: 523, DurationMS: 500},
		{FrequencyHz: 494, DurationMS: 500},
		{FrequencyHz: 440, DurationMS: 750},
		{FrequencyHz: 698, DurationMS: 250}, // F5
		{FrequencyHz: 698, DurationMS: 250},
		{FrequencyHz: 659, DurationMS: 500},
		{FrequencyHz: 523, DurationMS: 500},
		{FrequencyHz: 587, DurationMS: 500},
		{FrequencyHz: 523, DurationMS: 750},
	},
	songOdeToJoy: {
		{FrequencyHz: 330, DurationMS: 250}, // E4
		{FrequencyHz: 330, DurationMS: 250},
		{FrequencyHz: 349, DurationMS: 250}, // F4
		{FrequencyHz: 392, DurationMS: 250}, // G4
		{FrequencyHz: 392, DurationMS: 250},
		{FrequencyHz: 349, DurationMS: 250},
		{FrequencyHz: 330, DurationMS: 250},
		{FrequencyHz: 294, DurationMS: 250}, // D4
		{FrequencyHz: 262, DurationMS: 250}, // C4
		{FrequencyHz: 262, DurationMS: 250},
		{FrequencyHz: 294, DurationMS: 250},
		{FrequencyHz: 330, DurationMS: 250},
		{FrequencyHz: 330, DurationMS: 380},
		{FrequencyHz: 294, DurationMS: 120},
		{FrequencyHz: 294, DurationMS: 500},
	},
}

func notesForSong(song string) ([]Note, error) {
	notes, ok := songCatalog[song]
	if !ok {
		return nil, errors.New("unknown song")
	}
	out := make([]Note, len(notes))
	copy(out, notes)
	return out, nil
}

func validateNote(note Note) error {
	if note.FrequencyHz < 1 || note.FrequencyHz > 4000 {
		return fmt.Errorf("frequency_hz must be an integer from 1 to 4000")
	}
	if note.DurationMS < 10 || note.DurationMS > 1200 || note.DurationMS%10 != 0 {
		return fmt.Errorf("duration_ms must be an integer from 10 to 1200 and a multiple of 10")
	}
	return nil
}
