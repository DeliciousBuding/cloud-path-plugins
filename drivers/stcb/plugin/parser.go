// Package plugin 实现 CloudPath Driver Protocol v1 的 STC-B 驱动。
//
// STC-B（IAP15F2K61S2，UART 9600 8N1）线协议解析：状态转储 S 帧、全量传感器
// V 帧、事件行，以及设备钟漂移/热敏温度换算。本包只依赖公开 SDK
// （sdk/go/cloudpath/v1/*）与标准库/串口第三方库，不 import 任何
// github.com/DeliciousBuding/cloud-path/internal/**。
package plugin

import (
	"math"
	"regexp"
	"strings"
	"time"
)

// Dump 是一条状态转储的解析结果。
// 线上格式：S:<state><hour BCD 2位><min BCD 2位><三槽各1位>，共 8 位十进制半字节。
type Dump struct {
	State int    // 0=待机 1=提醒中 2=逾期
	Hour  int    // 软件 hour（0-23）
	Min   int    // 芯片 min（0-59）
	Slots [3]int // 每槽 0=待确认 1=已确认 2=逾期
	Raw   string // 原始行（取证）
}

// dumpRe 与板测工具同源：允许 S 与 8 位数字之间出现损坏字符（如 ':' 变 U+FFFD）。
var dumpRe = regexp.MustCompile(`S[:\x{FFFD}]?(.{8})`)

// ParseDump 解析转储行。响铃期损坏行/非转储行返回 false（上层忽略即可）。
func ParseDump(line string) (Dump, bool) {
	if !strings.Contains(line, "S") {
		return Dump{}, false
	}
	m := dumpRe.FindStringSubmatch(line)
	if m == nil {
		return Dump{}, false
	}
	d := m[1]
	for i := 0; i < 8; i++ {
		if d[i] < '0' || d[i] > '9' {
			return Dump{}, false
		}
	}
	dump := Dump{
		State: int(d[0] - '0'),
		Hour:  int(d[1]-'0')*10 + int(d[2]-'0'),
		Min:   int(d[3]-'0')*10 + int(d[4]-'0'),
		Slots: [3]int{int(d[5] - '0'), int(d[6] - '0'), int(d[7] - '0')},
		Raw:   line,
	}
	// 语义合法性：hour<=23 min<=59 槽位<=2（损坏数字行大概率越界，直接拒绝）
	if dump.Hour > 23 || dump.Min > 59 || dump.State > 2 {
		return Dump{}, false
	}
	for _, s := range dump.Slots {
		if s > 2 {
			return Dump{}, false
		}
	}
	return dump, true
}

// 线上标签 → 规范化事件类型。不同固件版本可能用缩短标签（BOOT/LATE/OK），
// 也可能带厂商前缀（如 "XXXX-BOOT"）——由 ParseEvent 的包含匹配兜底归一。
// 匹配顺序敏感：长标签在前，防止 TAKEN-LATE 被 TAKEN 抢先截胡。
var eventTags = []struct {
	tag       string
	canonical string
}{
	{"TAKEN-LATE", "TAKEN-LATE"},
	{"REMIND", "REMIND"},
	{"TAKEN", "TAKEN"},
	{"MISSED", "MISSED"},
	{"BOOT", "BOOT"},
	{"LATE", "TAKEN-LATE"},
	{"OK", "SYNC-OK"},
}

// ParseEvent 把事件行规范化（缩短标签与全名归一）。非事件行返回 false。
func ParseEvent(line string) (string, bool) {
	t := strings.TrimSpace(line)
	for _, e := range eventTags {
		if t == e.tag {
			return e.canonical, true
		}
	}
	// 容忍前后缀噪声（如 "[RAW-NOEOL] REMIND"、"VENDOR-BOOT"）：包含匹配
	for _, e := range eventTags {
		if strings.Contains(t, e.tag) && !strings.HasPrefix(t, "S") {
			return e.canonical, true
		}
	}
	return "", false
}

// StateLabel 状态机中文标签（设备无关的业务语义由适配器负责本地化）。
func StateLabel(s int) string {
	switch s {
	case 0:
		return "待机"
	case 1:
		return "提醒中"
	case 2:
		return "逾期"
	default:
		return "未知"
	}
}

// SlotLabel 槽位中文标签。
func SlotLabel(s int) string {
	switch s {
	case 0:
		return "待确认"
	case 1:
		return "已确认"
	case 2:
		return "逾期"
	default:
		return "未知"
	}
}

// BeijingNow 返回北京时间（Asia/Shanghai；tzdata 缺失时退化为固定 UTC+8）。
func BeijingNow() time.Time {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return time.Now().In(loc)
	}
	return time.Now().In(time.FixedZone("UTC+8", 8*3600))
}

// DriftMin 计算板钟相对北京时间的漂移（分钟，回绕到 [-720,720)）。
// 板钟只有时/分两位，精度天然 ±1min。
func DriftMin(hour, min int, bj time.Time) float64 {
	board := hour*60 + min
	now := bj.Hour()*60 + bj.Minute()
	diff := board - now
	diff = ((diff+720)%1440 + 1440) % 1440 // Go 负数取模防护
	diff -= 720
	return float64(diff)
}

// Sensor 是 V 帧全量传感器快照的解析结果。
// 线上格式：V:<hh BCD2><mm BCD2><ss BCD2><st hex1><rt hex3><rop hex3><nav hex3>
// <ext0 hex3><ext1 hex3><hall hex1><vib hex1><k1 hex1>，帧宽序与契约逐字节一致。
// 传感器值 = 原始 ADC/电平（0x000-0x3FF / 0-1），不做单位换算（换算归展示层）。
type Sensor struct {
	Hour  int    // 软件时 00-23
	Min   int    // 软件分 00-59
	Sec   int    // 软件秒 00-59
	State int    // 状态机 0=待机 1=提醒 2=逾期
	Rt    int    // 热敏 ADC 0x000-0x3FF
	Rop   int    // 光敏 ADC 0x000-0x3FF
	Nav   int    // 导航 ADC 0x000-0x3FF
	Ext0  int    // 扩展 P1.0 ADC 0x000-0x3FF
	Ext1  int    // 扩展 P1.1 ADC 0x000-0x3FF
	Hall  int    // 霍尔 0=无磁场 1=触发
	Vib   int    // 振动 0=静止 1=触发
	Key   int    // 按键 K1 0=未按 1=按下
	Raw   string // 原始行（取证）
}

// sensorRe 与板测工具同源：允许 V 与 25 个 hex 之间出现损坏分隔符（如 ':' 变 U+FFFD）。
var sensorRe = regexp.MustCompile(`V[:\x{FFFD}]?([0-9A-Fa-f]{25})`)

// bcd2 解析两位 BCD 十进制（0-9），越界/非法返回 false。
func bcd2(s string) (int, bool) {
	if len(s) != 2 || s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' {
		return 0, false
	}
	return int(s[0]-'0')*10 + int(s[1]-'0'), true
}

// hexVal 解析单个十六进制半字节字符。
func hexVal(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	}
	return 0, false
}

// hex3 解析 3 位十六进制（ADC 0x000-0x3FF 用）。
func hex3(s string) (int, bool) {
	if len(s) != 3 {
		return 0, false
	}
	a, ok1 := hexVal(s[0])
	b, ok2 := hexVal(s[1])
	c, ok3 := hexVal(s[2])
	if !ok1 || !ok2 || !ok3 {
		return 0, false
	}
	return a<<8 | b<<4 | c, true
}

// ParseSensor 解析 V 帧全量传感器快照。损坏行/越界值/非 V 帧返回 false（上层忽略即可）。
func ParseSensor(line string) (Sensor, bool) {
	if !strings.Contains(line, "V") {
		return Sensor{}, false
	}
	m := sensorRe.FindStringSubmatch(line)
	if m == nil {
		return Sensor{}, false
	}
	f := m[1]
	hh, okHH := bcd2(f[0:2])
	mm, okMM := bcd2(f[2:4])
	ss, okSS := bcd2(f[4:6])
	st, okSt := hexVal(f[6])
	rt, okRt := hex3(f[7:10])
	rop, okRop := hex3(f[10:13])
	nav, okNav := hex3(f[13:16])
	ext0, okExt0 := hex3(f[16:19])
	ext1, okExt1 := hex3(f[19:22])
	hall, okHall := hexVal(f[22])
	vib, okVib := hexVal(f[23])
	key, okKey := hexVal(f[24])
	if !okHH || !okMM || !okSS || !okSt || !okRt || !okRop || !okNav || !okExt0 || !okExt1 ||
		!okHall || !okVib || !okKey {
		return Sensor{}, false
	}
	// 语义合法性：BCD 越界或 ADC 越界/电平非 0/1 大概率是损坏行，直接拒绝。
	if hh > 23 || mm > 59 || ss > 59 || st > 2 || hall > 1 || vib > 1 || key > 1 {
		return Sensor{}, false
	}
	for _, v := range []int{rt, rop, nav, ext0, ext1} {
		if v > 0x3FF {
			return Sensor{}, false
		}
	}
	return Sensor{
		Hour: hh, Min: mm, Sec: ss, State: st,
		Rt: rt, Rop: rop, Nav: nav, Ext0: ext0, Ext1: ext1,
		Hall: hall, Vib: vib, Key: key, Raw: line,
	}, true
}

// TempC 由热敏 Rt 原始 ADC 换算摄氏温度。
// 依原理图：VCC→10K(R56)→V_Rt→Rt(10K/3950)→GND（10-bit ADC）：
// Rt = 10000*Adc/(1024-Adc)；Beta=3950，T0=25°C，R0=10K。
func TempC(rt int) float64 {
	if rt < 0 || rt > 1023 {
		return math.NaN()
	}
	r := 10000.0 * float64(rt) / (1024.0 - float64(rt))
	t := 1.0 / (1.0/298.15 + math.Log(r/10000.0)/3950.0)
	return t - 273.15
}
