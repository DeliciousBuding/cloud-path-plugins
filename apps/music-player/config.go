// SPDX-License-Identifier: Apache-2.0

package musicplayer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const maxConfigBytes = 4096

// Config is intentionally empty. The version 0.3.2 contract has no business
// settings: song and note parameters belong to the manual jobs, while output
// routing belongs to explicit capability bindings. Keeping a strict empty
// object makes future configuration changes deliberate instead of silently
// accepting unknown keys.
type Config struct{}

// Validate reports whether the version 0.3.2 configuration is valid. The
// empty struct has no business fields, so a successfully decoded object is
// always valid.
func (Config) Validate() error { return nil }

// UnmarshalConfig accepts an omitted config or a closed JSON object.
func UnmarshalConfig(raw []byte) (Config, error) {
	if len(raw) > maxConfigBytes {
		return Config{}, fmt.Errorf("config must be at most %d bytes", maxConfigBytes)
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return Config{}, nil
	}
	if !strings.HasPrefix(text, "{") {
		return Config{}, fmt.Errorf("config must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Config{}, fmt.Errorf("config must contain exactly one JSON object")
	}
	return cfg, nil
}
