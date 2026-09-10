# cloud-path-driver-stcb

STC-B（IAP15F2K61S2）的独立 CloudPath Driver Plugin。它把板载硬件翻译为 Device / Entity / Capability / Observation / Event / Command；CloudPath Core 不包含 STC-B、串口或药盒特例。

## Runtime chain

~~~text
CloudPath Edge → Plugin Host → cloud-path-driver-stcb → UART 115200 8N1 → STC-B Full Firmware v1.3.1
~~~

固件与协议位于 [stcb-firmware-sdk](https://github.com/DeliciousBuding/stcb-firmware-sdk)；正式线协议为 STC-B Device Protocol v1。

## Device UI contribution

Manifest 声明 `ui.apiVersion: 1` 的 `ui.device.sections`：状态、`source: diagnostics` 诊断和 `source: device-actions` 动作，追加到设备详情页。Driver 不注册业务主导航；设备能力与观测仍以 Descriptor / Capability 为事实源。

## Build and test

~~~bash
go build ./...
go vet ./...
go test ./... -count=1
python scripts/validate_manifest.py --self-test
python scripts/validate_manifest.py plugin.yaml --dir .
~~~

Driver 只依赖 CloudPath 公开 SDK，禁止 import cloud-path/internal/**。

## Edge configuration

同一个 Edge 可配置多块板；每块板必须有独立 device id、port 和本地生命周期：

~~~yaml
devices:
  - id: stcb-1
    adapter: stcb
    name: STC-B #1
    port: COM3
    baud: 115200
    extra: { protocol: v1 }
  - id: stcb-2
    adapter: stcb
    name: STC-B #2
    port: COM4
    baud: 115200
    extra: { protocol: v1 }
~~~

Edge 把每台设备的 port/baud/name/protocol 作为 OpenDevice connection hints 注入；ExecuteRequest.device_id 决定命令目标。后一块板不得覆盖前一块板的物理绑定。

### 多板目标路由

`entity_id` 是 Device 内的局部标识，不是跨板设备键。每块板都会声明 `buzzer` 等同名 Entity；平台设备身份始终是 `<edge_id>/<device_id>`，应用绑定的稳定目标是 `(device_id, entity_id)`。Driver 只接收 Edge 为本次目标注入的 `ExecuteRequest.device_id`，并在 `(PluginInstanceID, DeviceID)` 隔离的串口上执行，不会把请求发到“默认板”。

设备详情页和 REST/WS 命令按完整 `<edge_id>/<device_id>` 选择目标。Application binding 必须保存完整目标；Core 0.2.40+ 会按 `(device_id, entity_id)` 校验并路由。旧绑定只有 `entity_id` 时仅在唯一在线提供者下兼容；多板在线会 fail closed，并报 `entity ... is ambiguous across online devices: ...`。处理方式是重新绑定到目标设备，不能把 `EntityID` 改成 `stcb-real-1/buzzer` 绕过：Driver 不掌握 Edge 前缀，已发布的 `entity_id` 也必须保持稳定。

## Board capabilities

13 个稳定 Capability、16 个硬件 Entity：

- Clock：HH:MM:SS，每 10 分钟由 Driver 校时；
- Temperature、Illuminance；
- KN Navigation：ADC raw + 当前方向 + press/release event；
- EXT0 / EXT1 analog input；
- Hall：当前磁场电平 + close/away event；
- Vibration：当前电平 + quake event；
- K1 / K2 / K3：独立状态与 press/release event；
- Buzzer：档位提示音、Protocol v1 原始音调（`tone`）和本地音序（`tone-sequence`）；
- LED L0-L7：8-bit mask 独立控制；
- 8-digit display：HH-MM-SS、日期、传感器、I/O、版本五个自动页，以及数字/字母/空白/横线/小数点字形；
- Step motor connector；
- Board Diagnostics（`io.github.deliciousbuding/capability/board-diagnostics@1`：板级原始端口电平）。

板级诊断用**发布者命名空间**而不是 `cloudpath.dev/capability/diagnostics@1`：Core 的参考 demo
适配器占用后者且 action 集不同（`dump/noop/ping`），Server catalog 按 ID 去重会把本板的 `diag`
顶掉。第三方专有词汇走发布者命名空间是 capability-model 的既有规则。

Capability 的中文标题、动作标题与参数说明由本 Driver 的 Describe 声明；Core / WebUI 只消费声明，
不维护 STC-B 文案表。完整动作展示元数据需要 Core 0.2.13 或更新版本；旧 Core 仍可使用既有
动作与参数协议，但可能只显示动作标识。本版本在 Buzzer 能力上新增 `tone-sequence` 动作：Driver 本地按序执行，匹配内置旋律时自动走固件 `song` 原生音序器，旧固件仅在明确 `unknown` 时回退逐音 `beep`。

Alarm、compartment、服药时段不属于板载 Driver；这些业务只由 Application Plugin 通过 Capability 组合。

## Command truth

生产命令使用 CMD:<id>:<verb> 行协议。Driver 只有在收到相同 id 的板端 ACK 后才返回 SUCCEEDED；ERR 和 ACK timeout 都返回 FAILED。UART write success 绝不算设备执行成功。

- LED / Display：板端 API 执行后 ACK；
- Buzzer / Tone：实际发声完成后 ACK；
- Motor：实际转动完成后 ACK；
- 每块板命令串行，不同板可并发；
- 一块串口断开只关闭该设备的 Watch，Edge 单独重连，不影响同进程其他板。

命令面（唯一事实源 `plugin/command.go` 的 `supportedActions`）：
`buzzer` / `tone` / `tone-sequence` / `led` / `display` / `motor` / `sensor` / `sync` / `diag` / `isp` / `raw`。

- `diag` 只存在于 Protocol v1（`CMD:<id>:diag`）。legacy 单字符 `D` 在板上是 ISP 下载模式，
  Driver 不会把诊断命令编成 `D`（单测锁定）。`tone` 同样只存在于 Protocol v1；legacy 明确拒绝，不会降级成档位 `buzzer`。
- Legacy `V/B/L/N/T` 只用于旧探针固件 bring-up：没有关联 ACK，Driver 只诚实报告下发与回帧事实。
- 两种协议都在写 UART 前校验动作声明：`buzzer` 必须同时提供 `freq`（1–8）/ `duration`（0–8）；`motor` 必须提供
  `steps`；`tone` 必须且只能提供整数 `frequency_hz`（1–4000）/ `duration_ms`（10–1200，且为 10 的倍数）；`tone-sequence` 接受 1–64 个同样范围的音符和可选 `gap_ms`（0–1000，10 的倍数）；`led` 的 `mask` / `pattern` 和 `display` 的 `digits` / `codes` / `mode` 分别只能选一种；`display.mode` 可取 `clock`、`date`、`sensors`、`io`、`version`。
  缺值、`null` 或多个方案会返回错误；`buzzer.duration`、`motor`、`led.pattern` 等动作的显式 `0` 仍合法；`buzzer.freq` 与 `tone.frequency_hz` 的 0 越界。Legacy LED 仅支持 `pattern`；`mask` 须使用 v1。
- v1 `sync.time` 只接受有效 `HHMMSS`，`sync.hhmm` 只接受有效 `HHMM`；两者同时存在时沿用 `time`
  优先的兼容行为，空参数仍自动北京时间校时。Legacy `raw` 的 JSON 解码结果最多 64 UTF-8 字节，
  不接受 CR / LF / NUL；这些控制字符也不能进入 v1 命令 ID。
- 药盒业务动词（`dump` / `trigger` / `open`）与业务状态标签不属于硬件 Driver，已移除；
  这类语义只能由 Application Plugin 通过 Capability 组合表达。

## Manifest

- plugin id: io.github.deliciousbuding.cloud-path-driver-stcb
- driver id: stcb
- version: 0.2.12
- protocol: CloudPath Driver Protocol 1
- compatibility: CloudPath Core >=0.2.0 <0.3.0
- permission: hardware [serial]

硬件验收必须记录真实现象。当前没有接入步进电机实物时，Motor 只能标 implemented/tested，physical verification 必须保持 pending。
