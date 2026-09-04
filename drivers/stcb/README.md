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
- Buzzer；
- LED L0-L7：8-bit mask 独立控制；
- 8-digit display：HH-MM-SS、数字、空白/横线/H/L/小数点字形；
- Step motor connector；
- Diagnostics。

Alarm、compartment、服药时段不属于板载 Driver；这些业务只由 Application Plugin 通过 Capability 组合。

## Command truth

生产命令使用 CMD:<id>:<verb> 行协议。Driver 只有在收到相同 id 的板端 ACK 后才返回 SUCCEEDED；ERR 和 ACK timeout 都返回 FAILED。UART write success 绝不算设备执行成功。

- LED / Display：板端 API 执行后 ACK；
- Buzzer：实际发声完成后 ACK；
- Motor：实际转动完成后 ACK；
- 每块板命令串行，不同板可并发；
- 一块串口断开只关闭该设备的 Watch，Edge 单独重连，不影响同进程其他板。

命令面（唯一事实源 `plugin/command.go` 的 `supportedActions`）：
`buzzer` / `led` / `display` / `motor` / `sensor` / `sync` / `diag` / `isp` / `raw`。

- `diag` 只存在于 Protocol v1（`CMD:<id>:diag`）。legacy 单字符 `D` 在板上是 ISP 下载模式，
  Driver 不会把诊断命令编成 `D`（单测锁定）。
- Legacy `V/B/L/N/T` 只用于旧探针固件 bring-up：没有关联 ACK，Driver 只诚实报告下发与回帧事实。
- 药盒业务动词（`dump` / `trigger` / `open`）与业务状态标签不属于硬件 Driver，已移除；
  这类语义只能由 Application Plugin 通过 Capability 组合表达。

## Manifest

- plugin id: io.github.deliciousbuding.cloud-path-driver-stcb
- driver id: stcb
- version: 0.2.0
- protocol: CloudPath Driver Protocol 1
- compatibility: CloudPath Core >=0.2.0 <0.3.0
- permission: hardware [serial]

硬件验收必须记录真实现象。当前没有接入步进电机实物时，Motor 只能标 implemented/tested，physical verification 必须保持 pending。
