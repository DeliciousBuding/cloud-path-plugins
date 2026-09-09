# 霍尔药盒 2.0（Hall Pillbox）

把霍尔开盖传感器和可选 K1 按键变成药盒确认输入：按每日计划或管理台操作启动提醒窗口，记录按时确认、超时未确认和迟到确认，并跟踪蜂鸣器/显示命令的真实回执。

**版本 0.1.5；需要 Core >=0.2.15 且 <0.3.0。** 静音 `0/0` 的 `start-window → display reminder ACK → dashboard confirm → display idle ACK` 已完成生产真板 E2E，状态为 `VERIFIED`；物理磁铁 `opening/close-ignore/away` 序列仍需人工操作，状态为 `PLANNED`。仓库测试另覆盖内存协议、状态机、绑定和 effect 失败边界。

本应用是软件-only Application 插件：不访问 COM3、不启动/停止 Edge、不烧录、不修改现网配置。它只通过公开 SDK 请求已绑定的 Capability。

## WebUI 贡献

`plugin.yaml` 声明 `ui.apiVersion: 1`，Core 会为已安装实例生成导航“霍尔药盒”和独立路由 `/apps/hall-pillbox`。停用后入口仍保留，页面会明确显示当前未启用。页面由通用 section 组成：实例状态、来自 `window` 记录的指标、`window` 计划与记录时间线、手动操作和配置表单；`start-window`、`confirm-window`、`status` 作为手动动作展示，开盖确认仍以霍尔 `away`/兼容 `open` 事件为第一事实源。表单覆盖时区、药格标识和提醒音档位；日程数组与显示参数仍保留在高级详情。

## 1. 能力绑定

| Requirement | Capability | 数量 | 行为 |
|---|---|---:|---|
| `opening` | `cloudpath.dev/capability/hall@1` | one | 霍尔开盖/磁场边沿输入；Driver 的 `/away` 表示磁场离开（开盖），`/close` 表示磁场靠近（关盖）不触发确认；同时兼容通用 `/open` |
| `confirm` | `cloudpath.dev/capability/key@1` | zero-or-one | K1 兜底确认；未绑定时霍尔和管理台确认仍可用 |
| `reminder-output` | `cloudpath.dev/capability/buzzer@1` | one | 窗口开启时发出提醒，确认时发出停止命令 |
| `local-display` | `cloudpath.dev/capability/display-text@1` | zero-or-one | 可选视觉提示；确认后恢复 `idle_args`（通常是时钟） |

`compartment` 是应用自己的逻辑格 ID，不是 Driver、串口或按键实体 ID。

## 2. app_config

单 compartment 配置示例：

```json
{
  "timezone": "Asia/Shanghai",
  "compartment": "medicine",
  "schedule": [
    {"id": "morning", "start": "08:00", "end": "08:30"},
    {"id": "evening", "start": "20:00", "end": "20:30"}
  ],
  "reminder": {"freq": 4, "duration": 3},
  "display": {
    "reminder_args": {"digits": [0, 0, 0, 0, 0, 0, 0, 1]},
    "missed_args": {"digits": [0, 0, 0, 0, 0, 0, 0, 2]},
    "idle_args": {"mode": "clock"}
  }
}
```

字段规则：

- `timezone` 必须是有效 IANA 时区；窗口时间按该时区解释。
- `compartment` 必填，1–128 个 UTF-8 字节，无首尾空白。
- `schedule` 至少一项；`id` 唯一，`start/end` 必须是 `HH:MM`，`end` 晚于 `start`，不支持跨午夜。
- `schedule[].compartment` 是可选的扩展字段；当前版本只能为空或等于顶层 `compartment`。
- `reminder.freq`、`reminder.duration` 为 0–9 的整数。`0/0` 是静音约定：窗口开启和确认都不发 buzzer start/stop，`reminder_state` / `reminder_stop_state` 记录为 `suppressed`；可选 display 提示与恢复照常工作。其他取值在窗口开启时按配置发出 buzzer 命令，确认时发出 `{"freq":0,"duration":0}` 作为停止/静音命令。设备是否接受停止档由 `RequestCompleted` 原样记录，不伪造成功。
- `display` 可选。提供时必须有非空 JSON object 的 `reminder_args` 和 `idle_args`；`missed_args` 可选，省略时超时只更新窗口记录，不发送显示命令。参数属于绑定 Capability 的协议，应用不生成字形或厂商编码。
- 配置了 `display` 但没有绑定 `local-display` 时，绑定校验会给出 warning；应用不会猜测实体或发送显示命令。

Core 的每日窗口调度会读取配置中的 `schedule` 并发送 `ScheduleTick`。本应用以顶层单 `compartment` 为准，因此兼容 Core tick 中为空的 `compartment` 字段；后续扩展多 compartment 时再升级协议和状态机。

## 3. 状态机

```text
                    start-window / ScheduleTick
                               |
                               v
                           opened
                          /   |   \
                hall/key/dashboard  \  check-window 超时
                          |           v
                          v         missed
                      completed        \
                                      hall/key/dashboard
                                             |
                                             v
                                      completed_late
```

- `opened`：提醒窗口已开启，提醒命令状态初始为 `pending`。
- `completed`：在 `end` 前收到霍尔、K1 或管理台确认。
- `missed`：`check-window` 在 `now >= end` 后仍未收到确认。
- `completed_late`：`missed` 后收到开盖/确认，或在 `end` 时/之后才收到确认；原 `missed_at` 保留。
- `completed` / `completed_late` 只表示“收到了确认”，不证明药物已经吞服。
- 重复确认、重复 tick、重复检查不会重复开窗或重复生成设备命令。

## 4. 手动/自动 job

| Job | 参数 | 自动 | 说明 |
|---|---|---:|---|
| `start-window` | `{"window_id":"trial-001","minutes":10}` | 否 | 手动启动单 compartment 窗口；`minutes` 为 1–120；重复同一 ID/时长返回原结果，不重复发命令 |
| `confirm-window` | `{"window_id":"trial-001","source":"dashboard"}` | 否 | 管理台确认；`source` 必须为 `dashboard`，确认来源记录为 `dashboard` |
| `check-window` | `{}` 或 `{"window_id":"trial-001"}` | 是 | Core 每分钟调用；也可手动调用；扫描到期窗口并转为 `missed` |
| `status` | `{}` 或 `{"window_id":"trial-001"}` | 否 | 只读返回实例配置、活跃数和窗口记录，不发送设备命令 |

`start-window`、`confirm-window`、`status` 标记为 `ManualOnly`，避免旧 Core 的分钟调度器把管理台动作当自动任务。`check-window` 是唯一自动 job。

## 5. 事件和副作用

- `hall@1/away`（磁场离开，开盖）和兼容的 `hall@1/open`：绑定到 `opening` 的实体事件，视为霍尔开盖确认，`confirmation_source=hall`。`hall@1/close` 是磁场靠近/关盖，明确忽略，避免刚关盖或噪声边沿误确认。
- `key@1/press`：仅当 `confirm` 已绑定且实体匹配时作为兜底确认，`confirmation_source=key`。
- 管理台 `confirm-window`：`confirmation_source=dashboard`。
- 窗口开启：先写 `window` domain record，再发 buzzer（静音 `0/0` 时跳过）和可选 display 命令。
- 确认：写 `completed` / `completed_late` 记录，发 buzzer stop（静音 `0/0` 时跳过）和可选 display idle 命令。
- 超时：写 `missed` 记录，并在配置了 `missed_args` 时发 display missed 命令。
- 所有 `RequestCommand` 都有稳定幂等键：
  - `buzzer-start:<window_id>`
  - `buzzer-stop:<window_id>`
  - `display-reminder:<window_id>`
  - `display-missed:<window_id>`
  - `display-idle:<window_id>`
- 最终 `RequestCompleted` 只接受匹配的 request ID、entity、action 和当前 pending 命令；`succeeded/failed/timedout/cancelled` 写入窗口记录，`accepted/running` 不当作成功。

## 6. Domain record

`record_type=window`，`record_id` 为窗口 ID。记录至少包含：

`id`、`state`、`reminder_state`、`opened_at`、`closed_at`、`confirmation_source`、`schedule_id`、`compartment`。

同时保留 `start/end`、`source`、`missed_at`、`confirmed_at`、霍尔/按键实体、提醒命令 request/result/error、停止命令状态，以及 display 的 target/state/request/result/error。手动窗口的 `schedule_id` 为空；日程窗口的运行 ID 形如 `schedule:morning:20260908T000000Z`，`schedule_id` 保留配置中的 `morning`。

## 7. 测试

测试全部使用内存 fake event reader / effect writer，不访问真实板、串口、Edge 或网络：

```bash
go test ./... -count=1
go vet ./...
go build ./...
gofmt -l .
python scripts/validate_manifest.py --dir . plugin.yaml
```

覆盖点包括：配置与绑定 cardinality、启动副作用和幂等键、静音 `0/0` 不产生 buzzer start/stop 且保留 display 提示、霍尔 `away`/兼容 `open` 确认、`close` 忽略及 missed 窗口不被 close 迟到确认、错误 requirement/entity 隔离、K1/管理台确认、missed 与 completed_late、RequestCompleted 回执、重复 check/job 幂等、ScheduleTick 单 compartment 映射，以及 `HandleEvents` 的 fake reader/writer 路径。

### 7.1 手动真板 E2E（不会自动执行）

`scripts/e2e_hall_pillbox.py` 是人工触发的真板验收脚本：只走 CloudPath REST API，不直接打开 COM3、不启动/停止 Edge，也不打印凭据值。默认是 dry-run；只有显式 `--execute` 且处于交互式终端时才会执行写操作。该流程会发 buzzer 命令，因此默认 fail-closed：还必须显式传入 `--allow-audible`，否则在任何 API 写入前拒绝执行。脚本会创建独立实例，覆盖 `start-window`、`close` 不确认、`away`/兼容 `open` 确认、领域记录和停止命令回执，并在 `finally` 删除隔离实例。

```bash
python scripts/e2e_hall_pillbox.py                 # 仅打印计划，不连接网络
python scripts/e2e_hall_pillbox.py --execute --takeover-box-prod --allow-audible
```

`box-prod` 当前占用同一 `buzzer` 时，脚本默认拒绝运行并给出冲突提示；确认要临时接管生产实例后再加 `--takeover-box-prod`，脚本会在结束后恢复其原 `enabled` 状态。真板事件必须由操作者移动磁铁产生：先保持 away（开盖）启动窗口，再把磁铁靠近产生 `close`（应保持 `opened`），最后移开产生 `away`/`open`（应变为 `completed`、`confirmation_source=hall`）。本脚本不接入 CI 或无人值守任务。

## 8. 与 Scheduled Compartment 的边界

`scheduled-compartment` 用按键/多 compartment 表达取药确认，适合已有按键的格子；本应用把霍尔开盖作为第一确认源，把 K1 作为兜底，适合“开盖即确认”的单药盒。

两者都只依赖公开 Capability，不引用 Driver ID、端口或板卡型号。不要把 STC-B、COM3、A3144 极性或厂商串口协议写进本应用；这些属于 Driver/部署层。若未来需要多药格、每格独立霍尔或每格独立显示，应升级配置结构和状态机，而不是在 Core 中增加药盒特例。
