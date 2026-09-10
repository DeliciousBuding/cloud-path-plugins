// Package plugin 实现 CloudPath Driver Protocol v1 的 STC-B 驱动。
//
// STC-B（IAP15F2K61S2）线协议解析：Device Protocol v1 帧（见 protocol_v1.go）与
// legacy 探针 V 帧（bring-up 兼容，UART 9600 8N1），以及热敏温度换算。
// 本包只表达板载硬件事实，不含任何业务语义（药盒/分格/提醒归 Application Plugin）。
// 只依赖公开 SDK
// （sdk/go/cloudpath/v1/*）与标准库/串口第三方库，不 import 任何
// github.com/DeliciousBuding/cloud-path/internal/**。
package plugin

import (
	"math"
	"regexp"
	"strings"
	"time"
)

// BeijingNow 返回北京时间（Asia/Shanghai；tzdata 缺失时退化为固定 UTC+8）。
func BeijingNow() time.Time {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return time.Now().In(loc)
	}
	return time.Now().In(time.FixedZone("UTC+8", 8*3600))
}

// Sensor 是 V 帧全量传感器快照的解析结果。
// 线上格式：V:<hh BCD2><mm BCD2><ss BCD2><st hex1><rt hex3><rop hex3><nav hex3>
// <ext0 hex3><ext1 hex3><hall hex1><vib hex1><k1 hex1>，帧宽序与契约逐字节一致。
// 传感器值 = 原始 ADC/电平（0x000-0x3FF / 0-1），不做单位换算（换算归展示层）。
type Sensor struct {
	Hour int    // 软件时 00-23
	Min  int    // 软件分 00-59
	Sec  int    // 软件秒 00-59
	Rt   int    // 热敏 ADC 0x000-0x3FF
	Rop  int    // 光敏 ADC 0x000-0x3FF
	Nav  int    // 导航 ADC 0x000-0x3FF
	Ext0 int    // 扩展 P1.0 ADC 0x000-0x3FF
	Ext1 int    // 扩展 P1.1 ADC 0x000-0x3FF
	Hall int    // 霍尔 0=无磁场 1=触发
	Vib  int    // 振动 0=静止 1=触发
	Key  int    // 按键 K1 0=未按 1=按下
	Raw  string // 原始行（取证）
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
	st, okSt := hexVal(f[6]) // 帧内业务状态位：只做帧校验，不进硬件语义
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
		Hour: hh, Min: mm, Sec: ss,
		Rt: rt, Rop: rop, Nav: nav, Ext0: ext0, Ext1: ext1,
		Hall: hall, Vib: vib, Key: key, Raw: line,
	}, true
}

// TempC 由热敏 Rt 原始 ADC 换算摄氏温度。
// 依原理图：VCC→10K(R56)→V_Rt→Rt(10K/3950)→GND（10-bit ADC）：
// Rt = 10000*Adc/(1024-Adc)；Beta=3950，T0=25°C，R0=10K。
func TempC(rt int) float64 {
	if rt <= 0 || rt >= 1024 {
		return math.NaN()
	}
	r := 10000.0 * float64(rt) / (1024.0 - float64(rt))
	t := 1.0 / (1.0/298.15 + math.Log(r/10000.0)/3950.0)
	return t - 273.15
}
