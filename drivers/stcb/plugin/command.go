// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Cloudpath Authors

package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 执行器命令 key（Capability action 键 == 命令白名单命令名）。
const (
	actionBuzzer       = "buzzer"
	actionTone         = "tone"
	actionToneSequence = "tone-sequence"
	actionSong         = "song" // internal wire-only fast path; not an exposed action
	actionLED          = "led"
	actionDisplay      = "display"
	actionMotor        = "motor"
	actionSensor       = "sensor"
	actionSync         = "sync"
	actionISP          = "isp"
	actionRaw          = "raw"
	actionDiag         = "diag"
)

// supportedActions 是 Execute 支持的全部 action 白名单（唯一事实源）。
var supportedActions = []string{
	actionBuzzer, actionTone, actionToneSequence, actionLED, actionDisplay, actionMotor,
	actionSensor, actionSync, actionDiag, actionISP, actionRaw,
}

// slowActions 是需要逐字节慢发的命令：固件 UART 命令缓冲仅 1 字节，快发会丢字节。
var slowActions = map[string]bool{
	actionSync:         true,
	actionBuzzer:       true,
	actionTone:         true,
	actionToneSequence: true,
	actionLED:          true,
	actionDisplay:      true,
	actionMotor:        true,
}

// encodeCommand 把 action + argsJSON 编码为线协议字节帧。
// 返回的帧只含命令字节，不含换行（换行由 write 层按需追加）。
func encodeCommand(action, argsJSON string) ([]byte, error) {
	if err := validateActionArgs(action, argsJSON); err != nil {
		return nil, err
	}
	switch action {
	case actionISP:
		return []byte("D"), nil
	case actionSensor:
		return []byte("V"), nil
	case actionDiag:
		// 'D' 在板上是 ISP 下载模式，绝不能被诊断命令误触发；诊断只存在于 Protocol v1。
		return nil, fmt.Errorf("stcb: diag 需要 Protocol v1 固件（CMD:<id>:diag）")
	case actionTone:
		return nil, fmt.Errorf("stcb: tone requires Protocol v1 firmware (CMD:<id>:beep)")
	case actionToneSequence:
		return nil, fmt.Errorf("stcb: tone-sequence requires Protocol v1 firmware and is expanded by the device layer")
	case actionBuzzer:
		return encodeBuzzer(argsJSON)
	case actionLED:
		return encodeLED(argsJSON)
	case actionDisplay:
		return encodeDisplay(argsJSON)
	case actionMotor:
		return encodeMotor(argsJSON)
	case actionSync:
		return encodeSync(argsJSON)
	case actionRaw:
		return encodeRaw(argsJSON)
	default:
		return nil, fmt.Errorf("stcb: 不支持的命令 %q", action)
	}
}

// validateActionArgs enforces the existing required/oneOf declarations before
// either protocol can produce UART bytes. Value types/ranges stay in the encoders.
func validateActionArgs(action, argsJSON string) error {
	var required, choices []string
	switch action {
	case actionBuzzer:
		required = []string{"freq", "duration"}
	case actionTone:
		required = []string{"frequency_hz", "duration_ms"}
	case actionToneSequence:
		_, err := parseToneSequenceArgs(argsJSON)
		return err
	case actionSong:
		_, err := parseNativeSongArgs(argsJSON)
		return err
	case actionMotor:
		required = []string{"steps"}
	case actionLED:
		choices = []string{"mask", "pattern"}
	case actionDisplay:
		choices = []string{"digits", "codes", "mode"}
	default:
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(argsJSON), &fields); err != nil {
		return fmt.Errorf("stcb: %s args must be a JSON object: %w", action, err)
	}
	if fields == nil {
		return fmt.Errorf("stcb: %s args must be a JSON object", action)
	}
	if action == actionTone && len(fields) != 2 {
		return fmt.Errorf("stcb: tone accepts only frequency_hz and duration_ms")
	}
	for _, key := range required {
		value, ok := fields[key]
		if !ok || strings.TrimSpace(string(value)) == "null" {
			return fmt.Errorf("stcb: %s requires non-null %s", action, key)
		}
	}
	if len(choices) > 0 {
		selected := ""
		count := 0
		for _, key := range choices {
			if _, ok := fields[key]; ok {
				selected = key
				count++
			}
		}
		if count != 1 {
			return fmt.Errorf("stcb: %s requires exactly one of %s", action, strings.Join(choices, "/"))
		}
		if strings.TrimSpace(string(fields[selected])) == "null" {
			return fmt.Errorf("stcb: %s %s cannot be null", action, selected)
		}
	}
	return nil
}

func encodeBuzzer(args string) ([]byte, error) {
	var a struct {
		Freq     int `json:"freq"`
		Duration int `json:"duration"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return nil, fmt.Errorf("stcb: buzzer args 须为 JSON 对象（freq/duration）: %w", err)
	}
	if a.Freq < 1 || a.Freq > 8 {
		return nil, fmt.Errorf("stcb: buzzer freq 档须为 1-8，got %d", a.Freq)
	}
	if a.Duration < 0 || a.Duration > 8 {
		return nil, fmt.Errorf("stcb: buzzer duration 档须为 0-8，got %d", a.Duration)
	}
	return []byte{'B', byte('0' + a.Freq), byte('0' + a.Duration)}, nil
}

func encodeLED(args string) ([]byte, error) {
	var a struct {
		Pattern *int `json:"pattern"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return nil, fmt.Errorf("stcb: led args 须为 JSON 对象（pattern）: %w", err)
	}
	if a.Pattern == nil {
		return nil, fmt.Errorf("stcb: legacy led requires pattern; mask needs Protocol v1")
	}
	if *a.Pattern < 0 || *a.Pattern > 9 {
		return nil, fmt.Errorf("stcb: led pattern 档须为 0-9（0=灭 9=全亮），got %d", *a.Pattern)
	}
	return []byte{'L', byte('0' + *a.Pattern), '0'}, nil
}

func encodeDisplay(args string) ([]byte, error) {
	var a struct {
		Digits []int `json:"digits"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return nil, fmt.Errorf("stcb: display args 须为 JSON 对象（digits[8]）: %w", err)
	}
	if len(a.Digits) != 8 {
		return nil, fmt.Errorf("stcb: display digits 须为 8 个数字，got %d", len(a.Digits))
	}
	frame := []byte{'N'}
	for _, d := range a.Digits {
		if d < 0 || d > 9 {
			return nil, fmt.Errorf("stcb: display 数字须为 0-9，got %d", d)
		}
		frame = append(frame, byte('0'+d))
	}
	return frame, nil
}

func encodeMotor(args string) ([]byte, error) {
	var a struct {
		Steps int `json:"steps"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return nil, fmt.Errorf("stcb: motor args 须为 JSON 对象（steps）: %w", err)
	}
	if a.Steps < 0 || a.Steps > 4 {
		return nil, fmt.Errorf("stcb: motor steps 档须为 0-4（0=停），got %d", a.Steps)
	}
	return []byte{'M', byte('0' + a.Steps)}, nil
}

// encodeSync 编码对时帧 T+HHMM（4 位 ASCII）。hhmm 为空时用当前北京时间。
func encodeSync(args string) ([]byte, error) {
	hhmm := ""
	if args != "" {
		var a struct {
			HHMM string `json:"hhmm"`
		}
		if err := json.Unmarshal([]byte(args), &a); err != nil {
			return nil, fmt.Errorf("stcb: sync args 须为 JSON 对象（hhmm）: %w", err)
		}
		hhmm = a.HHMM
	}
	if hhmm == "" {
		hhmm = BeijingNow().Format("1504")
	}
	if !validHHMM(hhmm) {
		return nil, fmt.Errorf("stcb: sync args 须为 4 位 HHMM，got %q", hhmm)
	}
	return []byte("T" + hhmm), nil
}

func encodeRaw(args string) ([]byte, error) {
	var a struct {
		Args string `json:"args"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return nil, fmt.Errorf("stcb: raw args 须为 JSON 对象（args）: %w", err)
	}
	if a.Args == "" {
		return nil, fmt.Errorf("stcb: raw 命令需要 args")
	}
	if len(a.Args) > 64 || strings.ContainsAny(a.Args, "\r\n\x00") {
		return nil, fmt.Errorf("stcb: raw args must be <=64 UTF-8 bytes without CR/LF/NUL")
	}
	return []byte(a.Args), nil
}

func validHHMM(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	hh := int(s[0]-'0')*10 + int(s[1]-'0')
	mm := int(s[2]-'0')*10 + int(s[3]-'0')
	return hh <= 23 && mm <= 59
}

// wireByte 返回命令对应的固件单字节命令（仅用于结果摘要可读性）。
func wireByte(action string) string {
	switch action {
	case actionISP:
		return "D"
	case actionBuzzer:
		return "B"
	case actionLED:
		return "L"
	case actionDisplay:
		return "N"
	case actionMotor:
		return "M"
	case actionSensor:
		return "V"
	case actionSync:
		return "T"
	default:
		return "?"
	}
}

func validHHMMSS(s string) bool {
	return len(s) == 6 && validHHMM(s[:4]) && s[4] >= '0' && s[4] <= '5' && s[5] >= '0' && s[5] <= '9'
}
