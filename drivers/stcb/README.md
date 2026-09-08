# cloud-path-driver-stcb

STC-B（IAP15F2K61S2）的独立 CloudPath Driver Plugin。它把板载硬件翻译为 Device / Entity / Capability / Observation / Event / Command；CloudPath Core 不包含 STC-B、串口或药盒特例。

## Runtime chain

~~~text
CloudPath Edge → Plugin Host → cloud-path-driver-stcb → UART 115200 8N1 → STC-B Full Firmware v1
~~~

固件与协议位于 [stcb-firmware-sdk](https://github.com/DeliciousBuding/stcb-firmware-sdk)；正式线协议为 STC-B Device Protocol v1。

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

## Board capabilities

13 个稳定 Capability、16 个硬件 Entity：

- Clock：HH:MM:SS，每 10 分钟由 Driver 校时；
- Temperature、Illuminance；
- KN Navigation：ADC raw + 当前方向 + press/release event；
- EXT0 / EXT1 analog input；
- Hall：当前磁场电平 + close/away event；
- Vibration：当前电平 + quake event；
- K1 / K2 / K3：独立状态与 press/release event；
- Buzzer：档位提示音，以及 Protocol v1 的原始音调（`tone`）；
- LED L0-L7：8-bit mask 独立控制；
- 8-digit display：HH-MM-SS、数字、空白/横线/H/L/小数点字形；
- Step motor connector；
- Board Diagnostics（`io.github.deliciousbuding/capability/board-diagnostics@1`：板级原始端口电平）。

板级诊断用**发布者命名空间**而不是 `cloudpath.dev/capability/diagnostics@1`：Core 的参考 demo
适配器占用后者且 action 集不同（`dump/noop/ping`），Server catalog 按 ID 去重会把本板的 `diag`
顶掉。第三方专有词汇走发布者命名空间是 capability-model 的既有规则。

Capability 的中文标题、动作标题与参数说明由本 Driver 的 Describe 声明；Core / WebUI 只消费声明，
不维护 STC-B 文案表。完整动作展示元数据需要 Core 0.2.13 或更新版本；旧 Core 仍可使用既有
动作与参数协议，但可能只显示动作标识。本版本在 Buzzer 能力上新增 `tone` 动作，也不为普通执行器臆造破坏性标记。

Alarm、compartment、服药时段不属于板载 Driver；这些业务只由 Application Plugin 通过 Capability 组合。

## Command truth

生产命令使用 CMD:<id>:<verb> 行协议。Driver 只有在收到相同 id 的板端 ACK 后才返回 SUCCEEDED；ERR 和 ACK timeout 都返回 FAILED。UART write success 绝不算设备执行成功。

- LED / Display：板端 API 执行后 ACK；
- Buzzer / Tone：实际发声完成后 ACK；
- Motor：实际转动完成后 ACK；
- 每块板命令串行，不同板可并发；
- 一块串口断开只关闭该设备的 Watch，Edge 单独重连，不影响同进程其他板。

命令面（唯一事实源 `plugin/command.go` 的 `supportedActions`）：
`buzzer` / `tone` / `led` / `display` / `motor` / `sensor` / `sync` / `diag` / `isp` / `raw`。

- `diag` 只存在于 Protocol v1（`CMD:<id>:diag`）。legacy 单字符 `D` 在板上是 ISP 下载模式，
  Driver 不会把诊断命令编成 `D`（单测锁定）。`tone` 同样只存在于 Protocol v1；legacy 明确拒绝，不会降级成档位 `buzzer`。
- Legacy `V/B/L/N/T` 只用于旧探针固件 bring-up：没有关联 ACK，Driver 只诚实报告下发与回帧事实。
- 两种协议都在写 UART 前校验动作声明：`buzzer` 必须同时提供 `freq` / `duration`，`motor` 必须提供
  `steps`；`tone` 必须且只能提供整数 `frequency_hz`（1–4000）/ `duration_ms`（10–1200，且为 10 的倍数）；`led` 的 `mask` / `pattern` 和 `display` 的 `digits` / `codes` / `mode` 分别只能选一种。
  缺值、`null` 或多个方案会返回错误；既有 `buzzer` / `motor` 等动作的显式 `0` 仍合法，`tone` 的 0 越界。Legacy LED 仅支持 `pattern`；`mask` 须使用 v1。
- v1 `sync.time` 只接受有效 `HHMMSS`，`sync.hhmm` 只接受有效 `HHMM`；两者同时存在时沿用 `time`
  优先的兼容行为，空参数仍自动北京时间校时。Legacy `raw` 的 JSON 解码结果最多 64 UTF-8 字节，
  不接受 CR / LF / NUL；这些控制字符也不能进入 v1 命令 ID。
- 药盒业务动词（`dump` / `trigger` / `open`）与业务状态标签不属于硬件 Driver，已移除；
  这类语义只能由 Application Plugin 通过 Capability 组合表达。

## Manifest

- plugin id: io.github.deliciousbuding.cloud-path-driver-stcb
- driver id: stcb
- version: 0.2.4
- protocol: CloudPath Driver Protocol 1
- compatibility: CloudPath Core >=0.2.0 <0.3.0
- permission: hardware [serial]

硬件验收必须记录真实现象。当前没有接入步进电机实物时，Motor 只能标 implemented/tested，physical verification 必须保持 pending。
