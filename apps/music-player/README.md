# cloud-path-app-music-player

一个软件-only 的 CloudPath **Application 插件**，通过公开 Application SDK
把内置歌曲每轮重复压成一条 `tone-sequence` 命令，把单音压成一条 `tone` 命令，并维护当前 `music_session` 状态。
它不直接访问串口、浏览器、烧录工具或现网配置，只使用 Core 绑定后提供的实体 ID。

Version **0.2.4**；需要 Core `>=0.2.15 <0.3.0` 和公开 Go SDK v0.2.15。
状态：`IMPLEMENTED`。仓库测试使用 fake event stream / fake effect writer，
不是真板或现场验收证据。

## Web UI 贡献

Manifest 声明 `ui.apiVersion: 1`，安装实例后注册导航“音乐播放器”和独立路由 `/apps/music`。停用后入口仍保留，页面会明确显示当前未启用。首页由 Core 白名单 section 渲染：播放会话状态、来自 `music_session` 记录的指标、`play-song` / `play-note` 手动操作和 `music_session` 时间线；不注入 JavaScript、HTML 或远程资源。当前配置为空，因此不显示配置表单。

## Capability requirements

| Requirement | Capability | Cardinality | 用途 |
|---|---|---|---|
| `sound` | `cloudpath.dev/capability/buzzer@1` | `one` | 必须绑定；所有 `tone` / `tone-sequence` 命令都发往这个实体 |
| `local-display` | `cloudpath.dev/capability/display-text@1` | `zero-or-one` | 可选；当前版本只记录绑定可用性，为后续显示状态保留扩展点 |
| `indicator` | `cloudpath.dev/capability/led@1` | `zero-or-one` | 可选；当前版本只记录绑定可用性，为后续播放状态指示保留扩展点 |

绑定是显式的：应用不会自动选择按键、蜂鸣器、显示或 LED。`sound` 缺失、
重复或声明了未知 requirement 时，`ValidateBinding` 返回错误；播放 job 也会在
`FAILED_PRECONDITION` 下拒绝执行。两个可选绑定缺失时应用按纯声音模式降级，
`music_session` 中的 `local_display_bound`、`indicator_bound`、`degraded`
字段会反映实际状态。

## Configuration

版本 0.2.4 没有业务配置字段。`app_config` 必须是空 JSON object：

```json
{}
```

显式绑定示例（`app_bindings` 是 JSON 字符串，不是嵌套对象）：

```json
{
  "app_config": "{}",
  "app_bindings": "[{\"requirement_id\":\"sound\",\"entity_id\":\"BUZZER_ENTITY_ID\"}]"
}
```

需要可选能力时，在完整数组中追加对应绑定；不要只提交追加项：

```json
{
  "app_config": "{}",
  "app_bindings": "[{\"requirement_id\":\"sound\",\"entity_id\":\"BUZZER_ENTITY_ID\"},{\"requirement_id\":\"local-display\",\"entity_id\":\"DISPLAY_ENTITY_ID\"},{\"requirement_id\":\"indicator\",\"entity_id\":\"LED_ENTITY_ID\"}]"
}
```

## Manual jobs

三个 job 都是 `ManualOnly`，不会被自动分钟调度器当作后台任务执行。

| Job | Args | 约束 |
|---|---|---|
| `play-song` | `{"song":"little-star"|"birthday"|"ode-to-joy","repeat":1..3}` | 内置曲目必须存在；`repeat` 重复整首旋律 |
| `play-note` | `{"frequency_hz":1..4000,"duration_ms":10..1200}` | `duration_ms` 必须是 10 的倍数 |
| `status` | `{}` | 只读取并返回当前 `music_session`，不发送设备命令 |

`play-song` 的三个曲目名和 `play-note` 的数值边界同时写入
`JobDescriptor.InputSchemaJSON` 与运行时代码校验。未知 song、未知字段、
非整数、越界频率、非 10 倍数或越界时长都会在发出任何 effect 之前拒绝。

## 播放契约

对 `play-song`，应用按曲目定义的音符顺序展开 `repeat` 次，每一轮重复生成
一条 `tone-sequence` 命令；对 `play-note`，只生成一个 `tone` 音符命令。
一个实例同一时间只维护一个 `queued` / `playing` 会话；上一会话完成或失败前，
新的播放 job 会返回 `FAILED_PRECONDITION`。`tone-sequence` 的 Driver 侧契约是：

```json
{
  "action": "tone-sequence",
  "args_json": "{\"notes\":[{\"frequency_hz\":262,\"duration_ms\":300}, ...]}",
  "idempotency_key": "music-<session>:seq:<repeat-index>"
}
```

Driver 收到后在本机按序执行；当序列精确匹配内置曲目时走固件 `song` 原生
音序器，否则回退为逐音 `beep`。应用每次只提交**一条**在途命令，收到成功
`RequestCompleted` 后才提交下一轮重复；任一命令失败/超时/取消后，会话进入
`failed`，后续命令不再提交。应用**不会**在 `RunJob` 或事件处理里 sleep 阻塞，
也不直接操作硬件。

## `music_session` domain record

每次播放会先 upsert 一条 `record_type=music_session`、`record_id=current`
的 domain record。至少包含：

| 字段 | 含义 |
|---|---|
| `song` | `little-star` / `birthday` / `ode-to-joy` / `custom` |
| `status` | `idle`、`queued`、`playing`、`completed` 或 `failed` |
| `queued_at` | UTC RFC3339 时间戳 |
| `last_note` | 最近一个成功回执的音符对象；无成功回执时为 `null` |
| `repeat` | 本会话的重复次数；单音为 1 |
| `request_id` | 本会话稳定的请求 ID，也是每个音符幂等键的前缀 |

还会记录 `total_notes`、`completed_notes`、`error_code`、`result_json`、
`failed_note` 和可选绑定的降级标志。只有终态 `RequestCompleted` 会改变状态：
成功回执把 `queued` 推进为 `playing`，最后一个成功回执推进为 `completed`；
`failed`、`timed_out` 或 `cancelled` 会结束会话为 `failed`。重复或不属于当前
会话的回执会被忽略。

### Idempotency

同一个 `PluginInstanceID` 下，同一个非空 `idempotency_key` 和相同 job/args
重试时，`RunJob` 返回第一次的结果，不重复发送 domain record 或 `tone` 命令。
同一个 key 携带不同 job 或不同 args 会被拒绝。该缓存是当前插件进程的内存状态，
不是跨进程持久化存储；Core 重试仍应携带稳定 key，并由 Core/Driver 的命令幂等
机制共同保证端到端不重复。每一轮重复的 `tone-sequence` 命令 key 在会话内确定且唯一。

## 边界与降级

- 应用只依赖公开 SDK；没有 Driver ID、串口、COM3、Edge 启动、烧录或现网配置写入。
- `sound` 是唯一必需输出；可选 `local-display` / `indicator` 缺失不阻止播放。
- 0.2.3 不猜测 `display-text@1` 或 `led@1` 的 action/args 协议，因此即使绑定存在，
  当前版本也不会发送显示或 LED 命令；绑定可用性会出现在状态记录中。
- 不保证“命令已提交”等于“硬件已发声”；只有最终 `RequestCompleted` 才改变状态。
- 插件进程重启后，内存中的会话和幂等缓存不会自动恢复；`runtime_state_persistent=false`
  会出现在状态记录中。

## Tests

所有 Go 测试只使用 fake `ApplicationEventReader` / `ApplicationEffectWriter`，
不依赖真实板、串口或浏览器。

```bash
go test ./... -count=1
go vet ./...
go build ./...
gofmt -l .
python3 scripts/validate_manifest.py plugin.yaml --dir .
python3 scripts/validate_manifest.py --self-test
```

`gofmt -l .` 必须为空。manifest 校验器检查插件身份、三项 requirement 镜像、
公开 SDK pin、零权限和禁止 Core internal import。

## 后续 LED / display 扩展点

版本 0.2.3 已接受并保留两个可选绑定，但没有假定其硬件语义。后续版本可以在不改变
`tone` 契约的前提下增加：

- `local-display`：把 `queued`、`playing`、`completed`、`failed` 映射为明确的
  `display-text@1` action/args，并增加 fake writer 测试。
- `indicator`：把会话状态映射为明确的 `led@1` action/args，并明确失败时是否复位。
- 可选输出的缺失必须继续降级为声音-only，不得让显示或 LED 失败阻断已经排队的音符。
