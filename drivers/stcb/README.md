# cloud-path-driver-stcb

STC-B（IAP15F2K61S2，UART 9600 8N1）的 CloudPath Driver Plugin。
它是 CloudPath 的第一个官方 reference device：把 STC-B 学习板的串口协议翻译为
CloudPath 的 Capability / Observation / Event / Command 契约，并作为独立进程由
CloudPath Edge 的 Plugin Host 加载运行。

本仓库只依赖 CloudPath 公开 Go SDK（`sdk/go/cloudpath/v1/*`），
**不 import 任何 `github.com/DeliciousBuding/cloud-path/internal/**`**。

## 定位

```text
CloudPath Edge（通用运行时）
        ↓ Plugin Host 拉起本进程
cloudpath-driver-stcb（本仓库）
        ↓ UART 9600 8N1
STC-B 学习板（真实硬件）
```

CloudPath Core 不认识「STC-B」：本仓库贡献 driver id `stcb`，通过 manifest + 协议
把硬件带进平台。

## 布局

```text
cloud-path-driver-stcb/
  go.mod                        module github.com/DeliciousBuding/cloud-path-driver-stcb
  plugin.yaml                   Driver manifest（schema: spec/plugin-manifest.schema.json）
  config.example.json           插件实例配置示例
  cmd/cloudpath-driver-stcb/
    main.go                     handshake + 服务 Driver Protocol v1
  plugin/
    driver.go                   DriverServer 实现（Describe/Discover/Open/Watch/Execute/...）
    device.go                   串口 RX 循环 + 命令执行 + 真实回帧等待
    parser.go                   转储 S 帧 / 传感器 V 帧 / 事件行解析
    command.go                  命令编码（B/L/N/M/T/S/V/R/O/D）
    *_test.go                   黄金样本 + 命令编码 + 契约一致性
  scripts/validate_manifest.py  manifest + 无 internal import 门禁
  .github/workflows/            ci.yml + release.yml
```

## 依赖

| 依赖 | 说明 |
|---|---|
| Go ≥ 1.26 | 跟随 `go.mod`（`go 1.26.3`） |
| `github.com/DeliciousBuding/cloud-path v0.1.0` | 公开 Go SDK（Driver Protocol v1 / pluginmain） |
| `go.bug.st/serial` | 串口打开（第三方公开库） |

## 构建 / 测试

```bash
go build ./...
go vet ./...
go test ./... -count=1
python scripts/validate_manifest.py --self-test
python scripts/validate_manifest.py plugin.yaml --dir .
```

## 配置

插件实例配置（`config.example.json`）：

```json
{
  "device_id": "stcb-1",
  "name": "STC-B Board",
  "port": "COM3",
  "baud": 9600,
  "poll_interval_s": 5
}
```

| 字段 | 说明 |
|---|---|
| `device_id` | 设备稳定标识（缺省用 Host 下发的 DeviceID） |
| `name` | 展示名 |
| `port` | 串口（Windows `COM3`；Linux/macOS `/dev/ttyUSB0`） |
| `baud` | 缺省 9600 |
| `poll_interval_s` | 取帧周期（S 转储 + V 传感器），缺省 5 |

> Edge 部署时，此实例配置由 Server desired state 注入（不要假设 `edge.yaml` 的 `devices[].port` 会自动成为外部 Driver 的串口）。本地快速验证可 `cloudpath plugin enable <id> -config config.example.json` 后 `cloudpath plugin host`。

## 能力与实体

Driver id：`stcb`；插件 id：`io.github.deliciousbuding.cloud-path-driver-stcb`。

12 个 Capability（`cloudpath.dev/capability/` 命名空间，`@1`）：

```text
clock@1  alarm@1  contact@1  temperature@1  illuminance@1  hall@1
vibration@1  key@1  buzzer@1  led@1  display-text@1  motor@1
```

14 个 Entity：`clock`、`alarm`、`compartment-1..3`、`temperature`、`illuminance`、
`hall`、`vibration`、`key`、`buzzer`、`led`、`display`、`motor`。

执行器 action（`Execute` 白名单）：`buzzer` / `led` / `display` / `motor`；
生命周期命令：`sensor` / `dump` / `sync` / `trigger` / `open` / `isp` / `raw`。

## 串口协议速览

| 命令 | 线上写入 | 说明 |
|---|---|---|
| `sensor` | `V` | 请求 V 帧全量传感器快照（温度/光照/导航/扩展/霍尔/振动/按键/秒） |
| `dump` | `S` | 请求一次状态转储（`S:<state><hh><mm><3 slots>`） |
| `sync` | `T` + `HHMM` | 对时；逐字节 50ms 慢发（固件命令缓冲仅 1 字节） |
| `trigger` | `R` | 触发提醒 |
| `open` | `O` | 模拟一次确认动作 |
| `isp` | `D` | 延迟 5 秒软复位进 ISP 烧录模式 |
| `buzzer` | `B` + 2 数字 | 频率档 + 时长档（`{"freq":4,"duration":3}`） |
| `led` | `L` + 2 数字 | LED 档 0-9（0=灭 9=全亮），第二字节保留 0 |
| `display` | `N` + 8 数字 | 8 位数码管（`{"digits":[1..8]}`） |
| `motor` | `M` + 1 数字 | 步进电机档 0-4（0=停） |
| `raw` | `args` 原样 | 高级：直接写串口（长度 ≤64、不含换行/NUL） |

完整线协议契约见 CloudPath Core 的公开 `docs/protocol.md`。

## ACK/ERROR 语义

`Execute` 不把「串口写成功」当成「设备执行成功」：

- 执行器（`B/L/N/M`）固件 1S 拍执行、无独立回帧 → 返回下发事实摘要；
- `sensor`/`dump`/`sync`/`trigger`/`open`/`raw` 会等待**真实回帧**；未收到回帧时
  如实说明，不伪造成功；
- 结果详情经 `CommandProgress` 随 Watch 流回报，`ExecuteResponse` 带
  `SUCCEEDED` / `FAILED` 状态。

## 板级限制（驱动实现必须知道）

- 设备钟只有时/分两位，漂移精度天然 ±1 分钟；
- 响铃（蜂鸣）与串口 TX 同拍会互相干扰 → 解析器容忍损坏行；
- 长时间断电后 RTC 会被重置 → 打开设备后应对时。

## 安装 / 卸载

```bash
# 安装（digest 校验）
cloudpath plugin install github.com/DeliciousBuding/cloud-path-driver-stcb --digest sha256:<hex>

# 卸载
cloudpath plugin remove io.github.deliciousbuding.cloud-path-driver-stcb
```

## 机器身份契约（不要随意改）

一经发布即为稳定契约；破坏性语义变化升 `@2`，patch 不得静默扩大权限：

- plugin id：`io.github.deliciousbuding.cloud-path-driver-stcb`
- version / protocol：`0.1.0` / `1`
- entrypoint：`cloudpath-driver-stcb`
- driver contribution id：`stcb`
- permissions.hardware：`[serial]`（与实现一致，不得静默扩大）