# CloudPath Sensor Alert

`io.github.deliciousbuding.cloud-path-app-sensor-alert` 是一个设备无关的 CloudPath Application Plugin。它只消费绑定实体的观测和 CapabilityEvent，再通过领域记录与通用 `tone` / `led` 命令表达告警动作，不打开串口、不访问网络、不烧录固件。

版本：`0.1.0`
Application Protocol：`1`
最低 Core：`0.2.15`

## 与 Environment Guard 的边界

| 插件 | 职责 |
|---|---|
| `cloud-path-app-environment-guard` | 只读监测环境观测，维护环境状态与阈值变化记录；不发设备动作。 |
| `cloud-path-app-sensor-alert` | 在监测结果之上维护布防/告警状态机，并发出 sound/light 告警动作。 |

本插件不实现环境历史、曲线或设备采样；这些仍属于 Driver / Environment Guard / Core 的职责。它也不了解 STC-B、COM 口、串口帧或具体厂商字段。

## Requirements

所有 requirement 都是 `zero-or-one`，未绑定即不启用。配置可以进一步关闭 contact、vibration 或光照阈值。

| Requirement | Capability | 作用 |
|---|---|---|
| `temperature` | `cloudpath.dev/capability/temperature@1` | `property-observed` 的 `value` 越出温度上下限时告警。 |
| `illuminance` | `cloudpath.dev/capability/illuminance@1` | 配置了 `light_min` / `light_max` 时评估光照越界。 |
| `contact` | `cloudpath.dev/capability/hall@1` | `close` 触发，`away` 恢复；也接受 `state` 观测。 |
| `vibration` | `cloudpath.dev/capability/vibration@1` | `quake` 触发；`state=0` 或 `calm` 恢复。 |
| `alert-sound` | `cloudpath.dev/capability/buzzer@1` | 触发时发送 `tone` 命令。 |
| `alert-light` | `cloudpath.dev/capability/led@1` | 触发时发送 `led` 命令；恢复/撤防时发送 `mask=0`。 |

本插件声明 `hardware`、`network`、`filesystem`、`secrets` 权限全为空。

## app_config

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---:|---|
| `temperature_min` | number | `18` | 温度下限，必须小于上限。 |
| `temperature_max` | number | `28` | 温度上限。 |
| `light_min` | number / null | `null` | 光照下限；null 表示不评估该侧。 |
| `light_max` | number / null | `null` | 光照上限；null 表示不评估该侧。 |
| `contact_enabled` | boolean | `false` | 启用 hall 接触告警。 |
| `vibration_enabled` | boolean | `false` | 启用振动告警。 |
| `cooldown_s` | integer | `60` | 同一条件的重复触发冷却时间，范围 `0..86400`。 |
| `silent` | boolean | `false` | true 时仍记录和控灯，但不发 `tone`。 |
| `alert_led_mask` | integer | `255` | 触发时发给 LED 的 `mask`，范围 `0..255`。 |
| `alert_tone` | object / null | `{"frequency_hz":1000,"duration_ms":200}` | `tone` 参数；null 表示不发声。 |

`alert_tone` 约束与通用 tone primitive 一致：

- `frequency_hz`：`1..4000`
- `duration_ms`：`10..1200`，且为 10 的倍数

配置是封闭 JSON 对象；未知字段、重复字段、错误类型和越界值都会被拒绝。空配置使用默认值。示例见 `examples/app-config.json`。

## Jobs

| Job | 类型 | 行为 |
|---|---|---|
| `arm` | manual-only | 布防；重复调用不重复发 effect。 |
| `disarm` | manual-only | 撤防；清除活动条件，并让已绑定 LED 熄灭。 |
| `status` | manual-only | 返回当前配置、绑定、告警记录、待完成命令和最近命令结果。 |
| `check-freshness` | 自动 job | 只读返回绑定传感器的新鲜度报告，不采样、不触发告警。 |

所有 job 的 `args_json` 必须是空对象 `{}`。`arm` / `disarm` 支持 job idempotency key；同一 key 重放返回同一结果。`check-freshness` 使用插件内部 120 秒窗口，仅用于诊断，不改变告警状态。

## 告警状态机

```text
disarmed --arm--> armed
armed --threshold/event--> triggered
triggered --in-range/inverse event--> recovered
recovered --threshold/event--> triggered
armed/triggered/recovered --disarm--> disarmed
```

- 初始状态为 `disarmed`，不会在未布防时触发动作。
- 阈值、hall、vibration 都进入同一实例的 `triggered` 状态。
- 同一条件在 `cooldown_s` 内只触发一次；不同条件可以独立触发。
- `silent=true` 只抑制 sound，不影响领域记录和 light。
- 多条件同时活动时，只有所有活动条件恢复后才进入 `recovered`。
- `arm` / `disarm` 按实例幂等；不同 `plugin_instance_id` 的状态、冷却、命令和 effect writer 完全隔离。

### alert domain record

每个实例维护一条 `record_type=alert`、`record_id=current` 的当前/最近告警记录：

```json
{
  "sensor": "temperature",
  "state": "triggered",
  "value": 35,
  "threshold": 28,
  "triggered_at": "2026-09-08T12:00:00Z",
  "recovered_at": null,
  "severity": "warning",
  "summary": "temperature alert triggered (value=35, threshold=28, condition=high)"
}
```

`sensor` 可为 `temperature`、`illuminance`、`contact`、`vibration`；`state` 为 `armed`、`triggered`、`recovered`、`disarmed`。`contact` / `vibration` 的 `threshold` 为 null，severity 为 `critical`；阈值传感器为 `warning`。

## 命令与 RequestCompleted

触发时：

1. 先 upsert `alert` domain record；
2. 若绑定了 `alert-sound`、`silent=false` 且 `alert_tone != null`，发送 `RequestCommand{action:"tone", args_json: alert_tone}`；
3. 若绑定了 `alert-light`，发送 `RequestCommand{action:"led", args_json:{"mask":alert_led_mask}}`。

恢复或撤防时，LED 会收到 `{"mask":0}`。每条命令都有唯一 `idempotency_key` 和 30 秒 deadline。`RequestCompleted` 只有在 `state` 为 `succeeded` / `failed` / `timedout` / `cancelled` 等终态，且 `request_id`、`entity_id`、`action` 与待完成命令完全匹配时才会结算；非终态、未知或不匹配的完成事件会被忽略。命令失败/超时不会伪造恢复状态，也不会自动重试。

`tone` 需要目标 `buzzer@1` 实体声明并实现该 action。若目标 Driver 只提供 `buzzer` 档位命令，Core 会按正常命令失败路径返回结果；本 Application 不猜测或替换 Driver 的私有协议。

## 测试

软件-only 验收，不依赖 COM3、Edge、真实板或烧录：

```bash
gofmt -w .
go test ./... -count=1
go vet ./...
go build ./...
python scripts/validate_manifest.py --self-test
python scripts/validate_manifest.py plugin.yaml --dir .
```

测试使用 fake event/effect writer，覆盖：

- 配置默认值、nullable 字段和封闭校验；
- `arm` / `disarm` 幂等；
- 温度触发、冷却、恢复和再次触发；
- 光照 low/high、恢复和阈值边界；
- `silent` 只记录/控灯；
- hall / vibration CapabilityEvent，以及与同值 property observation 的去重；
- `RequestCompleted` 终态结算、非终态/错配事件忽略、失败/超时映射；
- 多活动条件全部清除后才进入 `recovered`；
- 多实例状态与 effect 路由隔离；
- `status` / `check-freshness` job；
- manifest、requirements 和公开 SDK import 边界。

## 真板 E2E（手动）

`scripts/e2e_sensor_alert.py` 是可选的手动真板验收工具，不是自动测试：

- 只走 CloudPath REST API；不直接打开 COM 口、不启动/停止 Edge、不烧录；
- 默认是 dry-run，必须显式 `--execute`，且要求交互式 TTY 和确认短语；
- 创建唯一的隔离实例，结束执行 `disarm` 并删除该实例；清理失败会以非零退出并写入 `cleanup_errors`，`--keep-instance` 仅用于排障；
- 凭据来自环境变量或 `--credentials-file`，脚本不打印凭据；
- 证据默认写入 gitignored 的 `.local/validation/`。

示例（请替换占位值）：

```bash
export CLOUDPATH_BASE_URL=https://<cloudpath-host>
export CLOUDPATH_E2E_DEVICE=<edge-id>/<device-id>
python scripts/e2e_sensor_alert.py --execute --sensor contact --recovery-mode disarm --credentials-file <path-to-key-value-file>
```

`--sensor` 支持 `temperature`、`illuminance`、`contact`、`vibration`。脚本会等待 domain record 进入 `triggered`，再等待 `tone` 和 `led` 的 device ACK，并通过 `status` job 确认 `RequestCompleted` 已清空 pending；随后按 `--recovery-mode` 验证 `recover` 或 `disarm` 的 LED off。若同租户已有实例独占 buzzer/LED，创建或绑定会失败；脚本不会自动停用其他实例，需操作者先显式释放执行器。

## 限制

- 本仓只实现 Application 层，不修改 Core、Driver 或其他 Application。
- 不持久化进程内状态；插件重启后需要重新 configure/bind/arm，Core 的 desired state 与记录仍由平台管理。
- `check-freshness` 只报告新鲜度，不把 stale 自动转成告警，避免在没有配置 stale 阈值时发明业务语义。
- 命令发送成功不等于设备执行成功；只有匹配的 `RequestCompleted` 才会更新最近命令结果。
- 真实硬件 E2E 需手动运行 `scripts/e2e_sensor_alert.py`；软件-only 验证不依赖 COM3、Edge、真实板或烧录。Driver `tone` 支持和跨租户生产验证仍需单独的真实链路证据。
- Application Protocol v1 的事件/RPC 只携带 `plugin_instance_id`，不携带 tenant。本插件按实例 ID 隔离状态，并对同一实例 ID 的第二个活动 effect stream 失败关闭；若部署允许不同租户复用同一实例 ID，必须使用 per-instance 隔离或保证实例 ID 跨租户唯一。
