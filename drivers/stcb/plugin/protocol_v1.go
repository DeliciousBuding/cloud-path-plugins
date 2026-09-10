package plugin

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// FullState is one STC-B Device Protocol v1 STATE frame.
type FullState struct {
	Clock                        string
	Hour, Min, Sec               int
	Temp, Light, Nav, Ext0, Ext1 int
	Hall, Vib, Key1, Key2, Key3  int
	NavKey                       int
	Motor, Beep, Display, Page   string
	LED                          int
	Raw                          string
}

// DeviceAck is a correlated ACK/ERR response from firmware.
type DeviceAck struct {
	ID     string
	OK     bool
	Detail string
}

func parseHex3(s string) (int, error) {
	if len(s) != 3 {
		return 0, fmt.Errorf("want 3 hex digits")
	}
	// STC-B ADC/温度字段是 10-bit（0x000–0x3FF）；0xFFF 等越界值不能伪装成有效观测。
	v, err := strconv.ParseUint(s, 16, 10)
	return int(v), err
}

func parseHexByte(s string) (int, error) {
	if len(s) != 2 {
		return 0, fmt.Errorf("want 2 hex digits")
	}
	v, err := strconv.ParseUint(s, 16, 8)
	return int(v), err
}

func parseRange(s string, max int) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 || v > max {
		return 0, fmt.Errorf("want 0-%d", max)
	}
	return v, nil
}

func parse01(s string) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil || (v != 0 && v != 1) {
		return 0, fmt.Errorf("want 0 or 1")
	}
	return v, nil
}

// ParseFullState parses a STATE:key=value,... frame. Unknown keys are ignored
// for forward compatibility; current capability fields are validated.
func ParseFullState(line string) (FullState, bool) {
	if !strings.HasPrefix(line, "STATE:") {
		return FullState{}, false
	}
	kv := map[string]string{}
	for _, part := range strings.Split(strings.TrimPrefix(line, "STATE:"), ",") {
		p := strings.SplitN(part, "=", 2)
		if len(p) == 2 {
			kv[p[0]] = p[1]
		}
	}
	clock := kv["clock"]
	if len(clock) != 8 || clock[2] != ':' || clock[5] != ':' {
		return FullState{}, false
	}
	hour, e1 := strconv.Atoi(clock[0:2])
	min, e2 := strconv.Atoi(clock[3:5])
	sec, e3 := strconv.Atoi(clock[6:8])
	if e1 != nil || e2 != nil || e3 != nil || hour > 23 || min > 59 || sec > 59 {
		return FullState{}, false
	}
	temp, e4 := parseHex3(kv["temp"])
	light, e5 := parseHex3(kv["light"])
	nav, e6 := parseHex3(kv["nav"])
	ext0, e7 := parseHex3(kv["ext0"])
	ext1, e8 := parseHex3(kv["ext1"])
	hall, e9 := parse01(kv["hall"])
	vib, e10 := parse01(kv["vib"])
	k1, e11 := parse01(kv["k1"])
	k2, e12 := parse01(kv["k2"])
	k3, e13 := parse01(kv["k3"])
	navKey, e14 := parseRange(kv["navkey"], 6)
	led, e15 := parseHexByte(kv["led"])
	if e4 != nil || e5 != nil || e6 != nil || e7 != nil || e8 != nil || e9 != nil || e10 != nil || e11 != nil || e12 != nil || e13 != nil || e14 != nil || e15 != nil {
		return FullState{}, false
	}
	return FullState{Clock: clock, Hour: hour, Min: min, Sec: sec, Temp: temp, Light: light, Nav: nav, Ext0: ext0, Ext1: ext1, Hall: hall, Vib: vib, Key1: k1, Key2: k2, Key3: k3, NavKey: navKey, Motor: kv["motor"], Beep: kv["beep"], Display: kv["display"], Page: kv["page"], LED: led, Raw: line}, true
}

// ParseDeviceAck parses ACK:<id>:ok and ERR:<id>:<code>.
func ParseDeviceAck(line string) (DeviceAck, bool) {
	ok := strings.HasPrefix(line, "ACK:")
	if !ok && !strings.HasPrefix(line, "ERR:") {
		return DeviceAck{}, false
	}
	p := strings.SplitN(line, ":", 3)
	if len(p) != 3 || p[1] == "" || strings.TrimSpace(p[2]) == "" {
		return DeviceAck{}, false
	}
	detail := strings.TrimSpace(p[2])
	if ok && detail != "ok" {
		return DeviceAck{ID: p[1], OK: false, Detail: detail}, true
	}
	return DeviceAck{ID: p[1], OK: ok, Detail: detail}, true
}

func ParseProtocolEvent(line string) (string, bool) {
	if !strings.HasPrefix(line, "EVENT:") {
		return "", false
	}
	body := strings.TrimSpace(strings.TrimPrefix(line, "EVENT:"))
	return body, body != ""
}

// NormalizeProtocolEvent maps wire events to capability-scoped event types.
func NormalizeProtocolEvent(body string) (entityID, eventType string) {
	switch body {
	case "hall=close":
		return "hall", capHall + "/close"
	case "hall=away":
		return "hall", capHall + "/away"
	case "vib=quake":
		return "vibration", capVib + "/quake"
	}
	if strings.HasPrefix(body, "key") {
		p := strings.SplitN(body, ":", 2)
		if len(p) == 2 {
			entity := p[0]
			return entity, capKey + "/" + p[1]
		}
		p = strings.SplitN(body, "=", 2)
		if len(p) == 2 {
			return p[0], capKey + "/" + p[1]
		}
	}
	if strings.HasPrefix(body, "nav=") {
		return "navigation", capNav + "/" + strings.TrimPrefix(body, "nav=")
	}
	return "", body
}

// ParseDiagnostic parses DIAG:key=value,... while ignoring unknown fields.
func ParseDiagnostic(line string) (map[string]string, bool) {
	if !strings.HasPrefix(line, "DIAG:") {
		return nil, false
	}
	out := map[string]string{}
	for _, part := range strings.Split(strings.TrimPrefix(line, "DIAG:"), ",") {
		p := strings.SplitN(part, "=", 2)
		if len(p) == 2 && p[0] != "" {
			out[p[0]] = p[1]
		}
	}
	return out, len(out) > 0
}

func encodeV1Command(id, action, argsJSON string) ([]byte, error) {
	if id == "" || strings.ContainsAny(id, ":\r\n\x00") {
		return nil, fmt.Errorf("stcb: invalid command id")
	}
	if err := validateActionArgs(action, argsJSON); err != nil {
		return nil, err
	}
	verb, args := action, ""
	switch action {
	case actionTone:
		var a struct {
			FrequencyHz int `json:"frequency_hz"`
			DurationMs  int `json:"duration_ms"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return nil, fmt.Errorf("stcb: tone args: %w", err)
		}
		if a.FrequencyHz < 1 || a.FrequencyHz > 4000 {
			return nil, fmt.Errorf("stcb: tone frequency_hz must be 1-4000")
		}
		if a.DurationMs < 10 || a.DurationMs > 1200 || a.DurationMs%10 != 0 {
			return nil, fmt.Errorf("stcb: tone duration_ms must be 10-1200 and a multiple of 10")
		}
		args = fmt.Sprintf("freq=%d,dur=%d", a.FrequencyHz, a.DurationMs/10)
		verb = "beep"
	case actionBuzzer:
		var a struct {
			Freq     int `json:"freq"`
			Duration int `json:"duration"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return nil, fmt.Errorf("stcb: buzzer args: %w", err)
		}
		if a.Freq < 1 || a.Freq > 8 || a.Duration < 0 || a.Duration > 8 {
			return nil, fmt.Errorf("stcb: buzzer freq must be 1-8 and duration 0-8")
		}
		args = fmt.Sprintf("freq=%d,dur=%d", freqTable[a.Freq], durationTable[a.Duration])
		verb = "beep"
	case actionLED:
		var a struct {
			Mask    *int `json:"mask"`
			Pattern *int `json:"pattern"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return nil, fmt.Errorf("stcb: led args: %w", err)
		}
		mask := 0
		if a.Mask != nil {
			if *a.Mask < 0 || *a.Mask > 255 {
				return nil, fmt.Errorf("stcb: led mask must be 0-255")
			}
			mask = *a.Mask
		} else if a.Pattern != nil {
			if *a.Pattern < 0 || *a.Pattern > 9 {
				return nil, fmt.Errorf("stcb: led pattern must be 0-9")
			}
			if *a.Pattern == 9 {
				mask = 255
			} else if *a.Pattern > 0 {
				mask = 1 << (*a.Pattern - 1)
			}
		} else {
			return nil, fmt.Errorf("stcb: led requires mask or pattern")
		}
		args = fmt.Sprintf("mask=%02X", mask)
	case actionDisplay:
		var x struct {
			Digits []int  `json:"digits"`
			Codes  []int  `json:"codes"`
			Mode   string `json:"mode"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &x); err != nil {
			return nil, fmt.Errorf("stcb: display args: %w", err)
		}
		if x.Mode != "" {
			switch x.Mode {
			case "clock", "date", "sensors", "io", "version":
				args = "mode=" + x.Mode
			default:
				return nil, fmt.Errorf("stcb: display mode must be clock/date/sensors/io/version")
			}
			break
		}
		if len(x.Codes) > 0 {
			if len(x.Codes) != 8 {
				return nil, fmt.Errorf("stcb: display codes must contain 8 values")
			}
			var out strings.Builder
			for _, code := range x.Codes {
				if code < 0 || code > 25 {
					return nil, fmt.Errorf("stcb: display code must be 0-25")
				}
				fmt.Fprintf(&out, "%02X", code)
			}
			args = "codes=" + out.String()
			break
		}
		if len(x.Digits) != 8 {
			return nil, fmt.Errorf("stcb: display digits must contain 8 values")
		}
		var out strings.Builder
		for _, digit := range x.Digits {
			if digit < 0 || digit > 9 {
				return nil, fmt.Errorf("stcb: display digit must be 0-9")
			}
			out.WriteByte(byte('0' + digit))
		}
		args = "digits=" + out.String()
	case actionMotor:
		var a struct {
			Steps int `json:"steps"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil || a.Steps < 0 || a.Steps > 4 {
			return nil, fmt.Errorf("stcb: motor steps must be 0-4")
		}
		if a.Steps == 0 {
			verb = "motorstop"
		} else {
			args = fmt.Sprintf("speed=100,steps=%d", a.Steps*50)
		}
	case actionSensor:
		verb = "state"
	case actionSync:
		verb = "sync"
		if argsJSON != "" {
			var a struct {
				Time string `json:"time"`
				HHMM string `json:"hhmm"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
				return nil, fmt.Errorf("stcb: sync args: %w", err)
			}
			if a.Time != "" {
				if !validHHMMSS(a.Time) {
					return nil, fmt.Errorf("stcb: sync time must be HHMMSS (000000-235959)")
				}
				args = "time=" + a.Time
			} else if a.HHMM != "" {
				if !validHHMM(a.HHMM) {
					return nil, fmt.Errorf("stcb: sync hhmm must be HHMM (0000-2359)")
				}
				args = "hhmm=" + a.HHMM
			}
		}
		if args == "" {
			args = "time=" + BeijingNow().Add(3*time.Second).Format("150405")
		}
	case actionSong:
		a, err := parseNativeSongArgs(argsJSON)
		if err != nil {
			return nil, err
		}
		verb = "song"
		args = "name=" + a.Song
	case actionToneSequence:
		return nil, fmt.Errorf("stcb: tone-sequence is expanded by the device layer")
	case "diag":
		verb = "diag"
	default:
		return nil, fmt.Errorf("stcb: protocol v1 does not support action %q", action)
	}
	line := "CMD:" + id + ":" + verb
	if args != "" {
		line += ":" + args
	}
	return []byte(line + "\r\n"), nil
}

var freqTable = [10]int{0, 500, 800, 1000, 1200, 1500, 2000, 2500, 3000, 0}
var durationTable = [10]int{5, 10, 15, 18, 25, 40, 60, 90, 120, 0}
