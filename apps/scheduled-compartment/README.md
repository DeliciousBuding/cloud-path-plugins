# 取药提醒（Scheduled Compartment）

把一个或多个按键变成**取药确认**输入：按每日计划或管理台操作开启提醒窗口，记录按时确认、到期未确认、迟到确认，以及提醒命令的实际回执。

**版本 0.2.3；需要 Core >=0.2.15 且 <0.3.0，公开 Go SDK v0.2.15。** 两个用户操作依赖 `JobDescriptor.ManualOnly`，不能放到不识别该字段的旧 Core 上运行，否则旧的分钟调度器可能把用户操作当自动任务。

状态：`IMPLEMENTED`。仓库测试覆盖内存协议、业务状态机和失败边界；不代表管理台 HTTP、真实设备或现场验收已经通过。应用不直接打开设备、串口或网络连接，只通过公开 SDK 请求已绑定的 Capability。

> `completed` / `completed_late` 仅表示**使用者确认取药**，不证明药物已经吞服。`opened` 是提醒窗口开始，不表示实体盒盖打开；`missed` 是截止时尚无取药确认，不足以断言使用者漏服。

## 1. 选择按键和配置

无需增加外接器件。一个按键即可承载一格的多次每日提醒；旧的三格配置也支持，但三格各需要一个不同按键。

| Requirement | Capability | 数量 / 行为 |
|---|---|---|
| `reminder-output` | `cloudpath.dev/capability/buzzer@1` | 必须一个；静音时仍走真实命令与回执链路 |
| `compartments` | `cloudpath.dev/capability/key@1` | 静态最少 1；实际绑定数必须等于配置格数 |
| `local-display` | `cloudpath.dev/capability/display-text@1` | 可选一个；配置显式 `display` 策略后提供静音视觉提示，无配置/绑定不输出 |

### 单板多应用：一格、一个按键

STC-B 同板验收时，给取药应用预留 **key1**；**key2、key3 不绑定到本应用**，留给呼叫应用。以下 `medicine` 是应用自己的逻辑格 ID，不是硬件编号。

```json
{
  "timezone": "Asia/Shanghai",
  "compartments": [{"id": "medicine", "name": "取药"}],
  "schedule": [
    {"id": "morning", "compartment": "medicine", "start": "08:00", "end": "08:30"},
    {"id": "evening", "compartment": "medicine", "start": "20:00", "end": "20:30"}
  ],
  "reminder": {"freq": 0, "duration": 0}
}
```

配置规则：

- `timezone` 必须是有效 IANA 时区；由 Core 时钟生成每日窗口。
- `compartments` 至少一项，`id` 为 1–128 个 UTF-8 字节、唯一、无首尾空白。数组顺序就是按键映射顺序。
- `schedule` 至少一项，`id` 为 1–128 个 UTF-8 字节且唯一，`compartment` 必须存在；`start/end` 必须严格为 `HH:MM`，结束晚于开始，不支持跨午夜。
- `reminder.freq/duration` 均为 0–9 整数。**省略整个 reminder 也默认 0/0 静音**；显式 0 保留，不会被替换成非零。非零提示音必须由操作人主动配置。

静音只关闭声音，不绕过提醒业务：仍会发出 `RequestCommand(buzzer)`，初态是 `pending`，需要 Core 回送最终 `RequestCompleted` 才能知道命令结果。

### 可选：静音视觉提示

如需在静音时仍有本地可见提示，在上面的 `app_config` 对象中增加 `display` 字段。以下仅为**可选示例**，需确认绑定实体的公开 `display-text@1 / display` 动作支持这些参数；应用不会猜文本协议或生成字形：

```json
{
  "display": {
    "reminder_args": {"digits": [0, 0, 0, 0, 0, 0, 0, 1]},
    "missed_args": {"digits": [0, 0, 0, 0, 0, 0, 0, 2]},
    "idle_args": {"mode": "clock"}
  }
}
```

这段是要合入完整配置的字段，不是替代整个配置。三个 `*_args` 均须为非空 JSON object，每个原始对象及规范化编码最多 2048 字节；缺字段、null、数组、字符串或超限对象会拒绝配置。参数内容属于所绑定 Capability 的协议，应用只校验对象与长度，不内置厂商字形或把提示状态转成硬件编码。

同时，在完整 `app_bindings` 数组里**追加**一条 `local-display` 绑定，保留原来的按键和提醒输出绑定：

```json
{"requirement_id": "local-display", "entity_id": "ENTITY_DISPLAY"}
```

当前验收所用实现的公开 schema 支持三选一：`{"digits":[八个0..9整数]}`、`{"codes":[八个0..25整数]}`、`{"mode":"clock"}`。上面的八位示例只表示**状态码 1＝待确认、2＝超时未确认**，不是第 1/2 格、药物数量或槽号；`clock` 是无待处理窗口时恢复的显示模式。是否支持这些参数由实际 Capability 实现决定。

显示选择按整个实例聚合，规则固定且简单：

| 未处理窗口 | 选择策略 |
|---|---|
| 至少一个 `missed` | `missed_args` 优先 |
| 没有 `missed`，但至少一个 `opened` | `reminder_args` |
| 没有上述两类窗口 | `idle_args` |

因此，确认一个窗口不会掩盖其余未处理窗口；迟到确认后，仍有待确认窗口就显示其待办状态，全部处理完才空闲。第一次窗口状态变化起才发送显示命令，空实例和不改变状态的分钟检查不重复写显示；只改配置也不会立即写设备，策略在下一次业务状态变化或在途显示的最终回执时应用。缺少 `display` 或 `local-display` 时不自动发任何显示/复位命令。

每个实例最多一条未终结的显示请求。等待 Core 的最终 `RequestCompleted` 后，才发送下一条；期间只保留最新期望状态，尚未发送的旧 idle 会被新提醒替换。已经发送的 idle 尚待回执时，新提醒排在其后；旧、重复或不匹配的回执不会覆盖新请求。这里保证本实例的提交顺序与回执关联，不替代真板对实际屏幕效果的验证；其他应用若共享同一显示实体，需由集成配置协调，本应用不跨实例/跨应用仲裁。

显示命令独立记录 `pending / succeeded / failed / timedout / cancelled`。仅提交请求不会被称为“已显示”，蜂鸣回执也不会释放显示队列。每条显示请求有 30 秒 deadline，由 Core 给出终态；若缺少最终回执，应用保留 `pending` 和最新待发状态，不自编成功/超时结果，也不会靠每分钟心跳重试同一状态。

网页可查看 `RecordType=display, RecordID=status` 的领域记录；只读状态及 Job 结果也有 `display` 对象：

- `desired_state`：期望的 `reminder / missed / idle`，不是已经显示的事实。
- `current`：当前请求的 ID、实体、实际 args、请求/截止时间，以及真实 `state / result_json / error_code / done_at`。
- `queued`：最新期望状态是否仍待提交。
- `last_completed`：最近一次终态；下一条请求变成 pending 时仍能看到前一条的真实结果。

取药窗口、蜂鸣提醒和显示请求是三件独立的事。显示成功不等于取药确认；取药确认也不会把仍 pending 的显示请求改成成功。视觉状态只增加现有进程内状态，不增加跨重启恢复机制。

### Core desired config 与精确绑定

在实例的 desired config 中，`app_config` 放上面的配置 **JSON 字符串**；`app_bindings` 放绑定数组的 **JSON 字符串**。这两个值不是嵌套对象/数组。例如最小单格配置：

```json
{
  "app_config": "{\"timezone\":\"Asia/Shanghai\",\"compartments\":[{\"id\":\"medicine\"}],\"schedule\":[{\"id\":\"morning\",\"compartment\":\"medicine\",\"start\":\"08:00\",\"end\":\"08:30\"}],\"reminder\":{\"freq\":0,\"duration\":0}}",
  "app_bindings": "[{\"requirement_id\":\"reminder-output\",\"entity_id\":\"ENTITY_BUZZER\"},{\"requirement_id\":\"compartments\",\"entity_id\":\"ENTITY_KEY_FOR_MEDICINE\"}]"
}
```

把示例中的 `ENTITY_*` 替换为 Core 实体目录里本租户的真实稳定 `entity_id`；名称叫 key1 的按键也必须使用它的实体 ID，不能直接填硬件端口或 Driver ID。保存并启动实例后，应先配置成功，再验证绑定成功。

- **存在 `app_bindings`**：Core 将整个数组当作精确绑定，验证租户、Capability、数量和重复项；非法时失败，**不会退回自动挑选**。
- **缺失 `app_bindings`**：保留旧的自动匹配。现在静态 `minItems=1`，自动匹配只选择最低所需数量；它既不保证选择 key1，也不会自动按三格配置补齐三键。因此同板多应用即使只有一格，也应明确绑定。
- 应用在 `ConfigureInstance` 检查已有绑定数量，在 `ValidateBinding` 检查实际配置数量；缺一、多一、重复键或未配置就绑定均失败。其他 Requirement 可穿插，筛选后的 `compartments` 绑定顺序必须与配置数组一致。

### 三格兼容与从 0.2.2 迁移

三格不需要修改程序：配置保留三个 `compartments`，并显式给 `app_bindings` 三条不同的按键实体，逐项对应：

| 配置数组位置 | 绑定数组中 compartments 的位置 |
|---|---|
| `compartments[0]` | 第一个按键实体 |
| `compartments[1]` | 第二个按键实体 |
| `compartments[2]` | 第三个按键实体 |

升级前，把依赖自动匹配的三格实例改成显式三键绑定，否则升级后的自动匹配只有一个键，会被实际数量校验拒绝，不能静默丢掉两格。只有 key1 可用时应改为一格，可把早/晚等多个日程都指向这一格；**不能把一个键伪装成三格独立确认**。

有 `opened` 窗口时拒绝热重排格 ID 或按键映射。已绑定实例改变格数时，配置与绑定这两个独立 RPC 都会拒绝数量不匹配，不能靠调换调用顺序绕过；应先核对旧窗口，再用**新实例**建立新格数及对应绑定。同格数的日程、名称和静音策略可更新，已开启窗口的起止时间不会被改写。不要靠随机自动重绑迁移，也不要把重启误当成恢复旧窗口的方法。

## 2. 管理台操作

Core >=0.2.15 的 `GET /api/plugin-instances/{id}/jobs` 在 `job_descriptors` 中提供标题、输入 schema 和 `manual_only`。执行入口：

```text
POST /api/plugin-instances/{id}/jobs/{job}/run
Content-Type: application/json

{"args_json":"<参数对象编码后的 JSON 字符串>","idempotency_key":"<本次逻辑操作的稳定键>"}
```

请求使用 Core 的正常登录/租户鉴权。Core 限制在实例声明的 Job 内并校验 schema；应用也会校验直接 SDK 调用的参数。输入 schema 为药格 ID、提醒分钟数和窗口 ID 提供中文标题及填写说明：药格 ID 来自配置，确认窗口 ID 应复制启动结果/窗口记录，不能相互替代。

| Job | 是否自动 | 参数 |
|---|---|---|
| `start-reminder` / 临时启动取药提醒 | 否，`ManualOnly=true` | `compartment_id`、整数 `minutes`（1–120）、稳定且唯一的 `window_id` |
| `confirm-window` / 确认指定窗口已取药 | 否，`ManualOnly=true` | 必填精确 `window_id`，不接受“当前格”“最新窗口”替代 |
| `window-check` / 检查到期未确认窗口 | 是 | `{}`；可带 `window_id` 作上下文，仍保持扫描全部到期窗口的旧语义 |

### 开一个一分钟现场验收窗口

在已登录管理台的浏览器控制台，可用同源请求调用；也可由管理台依据 schema 提供的表单执行。下面是操作示例，本仓库测试不会发这些网络请求：

```javascript
const instanceId = "替换为实例ID";
async function runJob(job, args, idempotencyKey) {
  const response = await fetch(
    "/api/plugin-instances/" + encodeURIComponent(instanceId) +
    "/jobs/" + encodeURIComponent(job) + "/run",
    {
      method: "POST",
      credentials: "same-origin",
      headers: {"Content-Type": "application/json"},
      body: JSON.stringify({
        args_json: JSON.stringify(args),
        idempotency_key: idempotencyKey
      })
    }
  );
  const result = await response.json();
  if (!response.ok) throw new Error(JSON.stringify(result));
  return result;
}

// 只生成一次；网络结果不确定时，重试必须复用这两个 ID。
const windowId = crypto.randomUUID();
await runJob("start-reminder", {
  compartment_id: "medicine", minutes: 1, window_id: windowId
}, "start:" + windowId);
```

Job 结果会给出 `window_id`、`source=manual`、开始/截止时间、`state=opened`、`reminder_request_id=reminder-<window_id>` 和 `reminder_state=pending`。`effects_status=submitted` 仅指 Effect 已提交到流，**不代表 Core 已持久化、设备已执行、使用者已取药或已服药**。

同一窗口 ID、格 ID、时长的重复启动不会延长窗口或重复提醒；同一 ID 换格/换时长会失败。同一 Job 的幂等键用于不同参数也会失败。手动 `window_id` 最多 128 个 UTF-8 字节（建议 UUID 或 ASCII ID），不能用保留前缀 `schedule:`。重复幂等键返回首次成功调用的结果快照；实时状态应看窗口记录，不把历史 `pending` 响应当成当前回执。

### 确认指定窗口

使用者确实确认取药后，按下绑定键，或者单独执行：

```javascript
await runJob("confirm-window", {window_id: windowId}, "confirm:" + windowId);
```

管理台确认记录 `confirmation_source=dashboard`；实体键确认记录 `confirmation_source=key`。两者都记录 `confirmed_at`，都不会把提醒命令的 `pending` 改成成功。

旧窗口 ID 的确认/重试只影响旧窗口，即使同一格已有新窗口。不存在的 ID 返回 `not_found`，不会创建窗口。已经确认的窗口不重写确认来源或时间。

## 3. 每日计划与状态证据

Core 按配置时区发送 `ScheduleTick`。每日计划与临时 Job 调用同一个开启函数，产生同样的窗口记录、真实提醒命令和窗口检查任务；来源分别为 `schedule` / `manual`。

日程窗口的运行 ID 为 `schedule:<日程规格ID>:<UTC开始时间>`，如 `schedule:morning:20260908T000000Z`。同一时刻的重复 tick 不重复提醒，次日则是不同窗口。记录中的 `schedule_id` 保留配置里的日程 ID。过期后才到达的 tick 只记录未按时确认，`reminder_state=not_requested`，不会补发过期提醒；提前到达或格式错误的 tick 被忽略。

| 窗口状态 | 含义 |
|---|---|
| `opened` | 在等待使用者确认；与提醒命令是否成功无关 |
| `completed` | 取药确认时间位于 `[start, end)` 内 |
| `missed` | 到期检查时尚未收到按时确认；记录 `missed_at`，发通知并取消检查任务 |
| `completed_late` | 确认时间达到或超过 `end`；保留截止时间和迟到确认时间 |

Core 每分钟运行 `window-check`，所以无确认的窗口通常在截止后的下一次分钟检查记为 `missed`，不是毫秒级定时器。但确认按自身时间戳判断：**即使检查还没运行，截止时或之后的按键/管理台确认也不会算准时**。延迟到达但发生在窗口内的按键事件仍按其真实 `occurred_at` 记准时。

同一格窗口重叠时，实体键选择该按键事件发生时已经开启的最新窗口；同开始时间按窗口 ID 确定顺序。已确认的新窗口会阻止重复按键退回去确认更旧的窗口；需要处理旧窗口时用它的精确 ID。其他格、其他 Requirement 或未绑定的按键不会确认本格。

提醒命令单独记录 `pending / succeeded / failed / timedout / cancelled`。只有窗口的请求 ID、输出实体、action 都匹配的最终 `RequestCompleted` 能设置结果；中间态、错误实体、重复/冲突终态不覆盖已结算结果。失败详情和 `error_code` 保留；命令成功本身也不改变取药确认状态。

窗口 domain record 自带应用生成的中文 `title/summary`，例如“早餐药格：待确认取药 / 已人工确认取药 / 已超时 / 迟到确认取药”。格 `name` 在窗口开启时捕获到 `compartment_name`，未填名称时标题使用格 ID；后续配置改名只影响新窗口，不会改写旧确认的名称。标题依据取药确认状态，不依据蜂鸣或显示 ACK，机器 `state` 和窗口 ID 保持原契约。

在 Core 的窗口 domain records / 命令结果中核对这些字段。应用只读状态子路由还提供数量和最近最多 100 个 `window_details`，以及独立的 `display` 状态；它不接受写操作。

## 4. 现场验收清单（默认全程静音）

1. 保存一格配置和显式 key1 + buzzer 绑定，`reminder` 为 0/0；要验收静音视觉提示，另外配置上面的 `display` 示例并绑定显示实体。实例配置与绑定都应成功。三格只绑一个键、绑定重复键或未知实体必须失败；不得自动换键。
2. 调用 `start-reminder`，检查窗口的格名/“待确认取药”标题和 `source=manual`；两个输出请求分别是 pending 或各自真实终态。启用示例策略时，板端应显示状态码 1；必须核对显示 ACK，不把“请求已发出”当作显示成功。同参数重试不得重复提示或推迟截止。
3. 截止前按 key1：`completed`、`confirmation_source=key`、标题“已人工确认取药”。若没有其他待办，显示在前一条请求终结后切换到示例的 clock，并独立等待回执。key2/key3 应只供其他应用使用，不改变本窗口。
4. 新开一分钟窗口，不确认，等待下一次检查：`missed` / “已超时”，示例显示状态码 2；随后按 key1：`completed_late` / “迟到确认取药”。有其他待确认窗口时回到其待办提示，否则空闲。在检查前、恰好截止时确认也必须是迟到。
5. 再开两个窗口，用旧窗口的精确 ID 在管理台确认；旧窗口记录 `dashboard`，新窗口仍为 `opened`。重复旧请求和不存在的 ID 都不得误确认新窗口。
6. 设置一个即将到来的每日窗口，等 Core 发 tick，核对 `source=schedule`、独立运行 ID 和相同的提示/回执链路；跨日使用新运行 ID。
7. 保留两个未处理窗口，只确认一个，显示不得直接空闲；在 idle 请求尚 pending 时再开窗口，新提醒必须排在旧请求终态之后。网页应区分 `desired_state`、`current.state` 和 `last_completed`，没有最终 ACK 时不能显示成成功。

单元/协议测试只能证明应用逻辑。上述真实 Core HTTP、实体事件路由、设备回执以及多应用共板互不干扰，仍需由集成/现场验收实际验证。

## 5. 明确边界：不伪造恢复或完成

窗口、Job 幂等缓存和事件序号保存在**应用进程内存**。公开 Application SDK 提供写 Effect 和接收事件，没有读取历史 domain records 或可靠重放检查点的恢复接口。本版本不具备跨进程重启的窗口恢复、离线日程补发或端到端 exactly-once 保证。

- 重启后旧窗口确认会返回 `not_found`；即使 Core 保留历史窗口记录/计划任务，也不表示应用已恢复该窗口。
- 跨重启不要盲目重放启动请求：应用已丢失去重缓存，Core 可能仍保存旧请求 ID。应先核对 Core 窗口、命令记录与实际使用情况，再由操作人决定后续动作。
- 无事件/Effect 流时，用户操作返回 `unavailable`，不创建窗口。Effect 批次部分发送失败时返回错误，并将实例标为交付不确定，后续写操作失败关闭；不会把重试包装成成功，也不会臆造恢复。需要集成侧/操作人核对已落地部分。
- 0.2.2 的日程记录使用未加日期的旧 ID；0.2.3 的新运行 ID 不会自动恢复或迁移这些历史记录。安排升级时先处理当前窗口并保留历史证据。

## 开发与离线验证

```bash
go test ./...
go vet ./...
python scripts/validate_manifest.py --self-test
python scripts/validate_manifest.py plugin.yaml --dir .
gofmt -l .
go build -o bin/cloud-path-app-scheduled-compartment ./cmd/cloud-path-app-scheduled-compartment
```

依赖正式发布的 Core public SDK v0.2.15；`go.mod` 不包含本地 `replace`。首次构建用 `go mod download` 获取经过校验的依赖。测试只使用内存传输和可控时钟，不调用设备、串口、生产网络或实际蜂鸣；Linux CI 额外运行 race detector。

主程序由 Core Plugin Host 注入启动身份并托管，不是独立运行的硬件程序。本仓不依赖 Core `internal/`，不定义 Driver/端口特例。

| 文件 | 职责 |
|---|---|
| `config.go` | 配置与静音策略 |
| `service.go` | 绑定、事件、窗口/回执记录、实例流路由 |
| `jobs.go` | 中文操作 schema、幂等、共用窗口转换和窗口展示文字 |
| `display.go` | 显式显示策略、单在途切换和独立回执 |
| `service_test.go` / `reminders_test.go` / `display_test.go` | 原有协议回归、提示切换及实际操作边界 |
| `manifest_test.go` | 版本、三处需求声明、ManualOnly 与 schema 一致性 |
| `plugin.yaml` / `requirements.yaml` | 机器清单与需求镜像 |
