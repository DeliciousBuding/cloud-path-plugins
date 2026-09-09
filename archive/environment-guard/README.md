# Environment Guard

设备无关的 CloudPath Application 插件，版本 **0.1.5**。将已绑定的温度、光照观测变成工位环境快照和阈值变化记录，不轮询设备，不请求设备动作，不发送系统外通知。

状态：**IMPLEMENTED**。应用代码、行为测试和 public SDK RPC 可本地验证；安装运行、Core 观测扇入、真实来源单位/光敏方向及板测仍由集成验收确认。此仓库未发布、未部署。

## Web UI 贡献

Manifest 声明 `ui.apiVersion: 1`，安装并启用实例后注册导航“环境监测”和独立路由 `/apps/environment`。首页由 Core 白名单 section 渲染：实例状态、来自 `environment/current` 记录的指标、手动刷新、`alert` 变化时间线和配置表单。读数值与动态单位在同一指标卡显示，`unit` 指向记录中的单位字段，不在 manifest 中伪造固定量纲。表单覆盖时区、温度/光照阈值、回差、过期时间和单位；不注入 JavaScript、HTML、远程资源或全局 CSS。原始配置 JSON 仅保留为高级入口。

## 默认做什么

- 温度在设定上下限之外时记录阈值进入；回到带 hysteresis 的恢复区间时记录恢复。
- **光照默认仅显示原始读数，阈值关闭。** 可选数字阈值支持 below/above；提示“低于/高于设定阈值”，不根据未知方向解释物理明暗。
- 只更新一个 `environment/current` 快照。同状态数字更新最多每 30 秒写一次；首条、每个传感器首条、状态/质量/绑定/配置变化立即写。
- durable scheduler 每分钟运行一次 `check-freshness`；过期后变成 `stale`，无效/缺失数据变成 `unknown`，不会把断流当作持续正常。
- 不需要额外器件或输出能力；可与同一设备上的其他应用共存。没有按键、声音、指示灯、显示输出的 requirement，更不会争抢输出。

这是学习/工位的自定义阈值提示，不是健康、安全、合格照度或校准精度判断。

## 安装前提与绑定

要求 **Core >=0.2.15 <0.3.0**，Application Protocol 1。v0.2.15 才提供本应用依赖的属性观测扇入和手动任务语义；不能只给旧 Core 放宽版本限制。

| Requirement ID | Capability | Cardinality |
|---|---|---|
| `temperature` | `cloudpath.dev/capability/temperature@1` | one |
| `illuminance` | `cloudpath.dev/capability/illuminance@1` | one |

本版**两项都必需**，包括光照阈值关闭时。每项必须恰好绑定一个真实实体；同一设备上的两个实体可以分别承担两项输入。当前 Core Binder 不允许同一实体重复占用两个角色；应用自身不依赖设备数量。Core Binder 负责能力匹配和租户授权，应用再次核对 requirement/entity/capability/property。未知、重复、空或缺少的绑定被拒绝。换绑会丢弃该角色旧读数，等待新实体观测；不会继承原实体的正常状态。

## 配置

将 [examples/app-config.json](examples/app-config.json) 的 JSON 作为 `ConfigureInstance.Config`。Server Application 实例的通用配置封装是：

```json
{
  "edge_id": "server",
  "instance_id": "desk-environment",
  "plugin_id": "io.github.deliciousbuding.cloud-path-app-environment-guard",
  "version": "0.1.5",
  "enabled": true,
  "config": {
    "app_config": "{\"timezone\":\"UTC\",\"temperature_min\":18,\"temperature_max\":28,\"light_threshold\":null,\"stale_after_s\":120}"
  }
}
```

这是给已安装 catalog 的 `POST /api/plugin-instances` 请求形状，不会替代安装。`app_config` 是 JSON **字符串**；里面没有实体选择器或硬件参数。用 `GET /api/plugin-instances/{id}/bindings` 检查 Core 的实际绑定，不能把示例实体名当作真实绑定。

| 配置字段 | 默认 | 校验/含义 |
|---|---|---|
| `timezone` | UTC | 显式 IANA 时区；拒绝依赖宿主的 Local；二进制嵌入 tzdata |
| `temperature_min` / `temperature_max` | 18 / 28 | 有限数字，min < max；与输入同单位，不转换 |
| `light_threshold` | null | null 或省略表示关闭；启用时必须是有限数字 |
| `light_alert_when` | below | below / above；是数字比较方向，不是校准结论 |
| `hysteresis.temperature` | 1 | >=0，且小于温度区间的一半 |
| `hysteresis.light` | 5 | 有限且 >=0，使用原始光照单位 |
| `stale_after_s` | 120 | 整数，60–86400 秒 |
| `temperature_unit` / `light_unit` | 空字符串 | 可选期望/缺省单位；最多 32 字符，不含控制字符 |

未知键、重复键、无效时区、字符串数字和不允许的 null 都拒绝。只有 `light_threshold` 明确允许 null。配置 schema 见 [config.schema.json](config.schema.json)；上下限/hysteresis 的跨字段关系由应用进一步校验。

### 光敏读数与量纲

本次集成来源的原始光敏 ADC 量程是 **0..1023**，不是百分比；光强变化时 ADC 增减方向尚需实测确认。本应用不硬编码该量程，更不把所有实现此 capability 的来源都当作同类传感器。

单位优先取观测的 `unit`；没有时使用明确配置的单位，否则光照展示 **“原始/相对读数”**，温度展示“未声明单位”。没有自动归一化或物理单位换算。配置了期望单位而收到不同单位，或运行中未配置的单位元数据发生变化，会标记 unknown；重新配置或换绑后用新观测确认，不能给旧值直接改单位。

在来源方向/量程确认前保持 `light_threshold:null`。确认后可例如设置：

```json
{
  "light_threshold": 300,
  "light_alert_when": "below",
  "hysteresis": {"light": 30}
}
```

该例仅表示数值 <300 进入、>=330 恢复；并不声称读数低就是环境暗。`above` 则 >300 进入、<=270 恢复。关闭或更换光照阈值/方向是配置变化，不伪装成一次测量证明的恢复。

## 观测与记录契约

只消费 `application.CapabilityEvent`：

- `EventType` **精确等于** `cloudpath.dev/event/property-observed@1`。
- `RequirementID` 为绑定角色，`EntityID` 为该角色绑定的真实实体。
- `PayloadJSON` 是 public `sdk/go/model.Observation` JSON，`capability` 必须匹配该角色，`property` 必须为 `value`，`value` 必须为有限 JSON number。
- 可选 `unit`、`quality`、`observed_at`、`received_at`、`sequence`；payload 若带 `entity_id`，也须与信封匹配。

`bad/unavailable/uncertain`、非法 quality、null、非数字、NaN/Infinity、非法/显著未来时间均不能成为正常状态。旧时间与重复/倒退 sequence 不覆盖新读数。新到达的坏数据会让该角色未知，而不是保留旧的正常外观。旧观测不能因最新 received_at 而“洗成”新鲜数据。

没有质量元数据时如实写 `unspecified`。没有采样时间时 `observed_at:null`，以真实应用接收时刻检查新鲜度，并通过 `application_received_at` / `freshness_basis` 区分；不伪造设备采样时间。`observed_at` 与 `received_at` 同时存在时使用较早者做保守新鲜度检查。

### 当前环境

`record_type=environment, record_id=current`，包含人可读 `title/summary/status`、温度/光照具体值及单位、`thresholds`、`quality`、真实 `observed_at`、`evaluated_at` 和配置 revision。

| status | 意义 |
|---|---|
| `within_thresholds` | 温度及已启用的数值阈值均在范围内；不表示健康/安全或光照已标定 |
| `attention` | 至少一个启用的阈值被触发 |
| `stale` | 至少一个必需观测过期 |
| `unknown` | 未配置/未绑定/未收到数据，或质量、数值、单位有问题 |

每个传感器单独保留 `status/quality/usable/threshold_enabled`。关闭光照阈值时，光照状态为 `unassessed`，显示值但不下阈值结论；该数据仍需新鲜。总体 unknown/stale 优先于阈值结论，其他仍有效的数值及阈值状态保留在各传感器字段中。过期值若仍显示，`usable=false`，并明确 stale，绝不显示成新采样。

### 阈值变化记录

`record_type=alert`。温度高/低、光照 below/above 各保留最近一次 entered/recovered，共 **最多 8 条固定 ID**；同方向重复读数、hysteresis 区间抖动不产生新 alert 写入。固定槽位被后续同类变化覆盖，不是无限历史。alert 是**过去的变化记录**，不是当前活动告警列表；当前事实以 environment/current 为准。

坏/过期读数不会产生“恢复”，也不会清除 hysteresis 的最后有效状态。本地待重试的 alert 同样最多 8 个。没有自建数据库；只有 Core 的领域记录存储。流断开时不把效果发送成功说成已经落库，Core/浏览器读面才是落库验收依据。

## 调度与手动刷新

- descriptor 的 `bootstrap` 仅幂等声明 `ScheduleTask{ScheduleID:"check-freshness", Cron:"* * * * *", PayloadJSON:"{}"}`。首条流事件也会声明；同配置/同连接不重复声明。
- `check-freshness` **不在 descriptor 的自动 job 列表**，只由 durable scheduler 调用 RunJob。断流但宿主正常时，最迟在 stale_after_s 后的下一分钟检查标 stale；宿主停止时不保证墙钟到点写入。
- `refresh-status` 在 descriptor 中为 `ManualOnly:true`，输入 schema 是不含额外字段的空对象。只重新计算已收到数据的新鲜度/阈值和限频快照；结果含 `sampled:false`。

手动操作通过 Core 通用接口，不是本插件自建 REST 动作：

```http
POST /api/plugin-instances/{id}/jobs/refresh-status/run
Content-Type: application/json

{"args_json":"{}","idempotency_key":"operator-refresh-unique-id"}
```

不同操作用新 key；重复 key 返回原结果（包括原 evaluated_at），不再执行写入。每实例最近 128 个 job 结果在进程内去重，配置/绑定改变后清空；不宣称跨重启 exactly-once。缺少可用的实例 effect stream 或发送失败会报错，不把任务记成成功。

只读插件子路由 GET `/status` 返回实时重算结果，不写记录。`freshness_schedule_declared` 只说明已发送声明；实际 durable task 见 `GET /api/plugin-instances/{id}/jobs` 的 scheduled 字段。

## 构建、安装与验收

Go 1.26.3，Python 3（校验器仅 stdlib）。plugin.yaml/requirements.yaml 使用 JSON 兼容 YAML，便于零依赖完整校验，Core 的 YAML 解析器可读取。正式 `go.mod` 固定 Core v0.2.15，无本地 replace。若 v0.2.15 尚未发布，需要在**临时、gitignored 的 modfile** 中引用集成方提供的 Core 工作树验证；这不能替代发布版依赖验证。发布版可用后运行 go mod download，核对并提交新生成的 go.sum；不能拿其他版本的校验和代替。

```bash
go mod download
go test ./... -count=1
go vet ./...
go build ./...
gofmt -l .
python scripts/validate_manifest.py --self-test
python scripts/validate_manifest.py plugin.yaml --dir .
go build -trimpath -o bin/cloud-path-app-environment-guard ./cmd/cloud-path-app-environment-guard
```

Windows 最后一条输出名加 .exe。插件由 Host 注入身份/传输启动，使用 public `pluginmain.Run + application.NewRPCServer`；不自行选择端点，无独立设备、网络、文件或 secret 权限。

准备安装物时需同版本 `plugin.yaml`、本平台入口二进制及 SHA-256。工作流 [release.yml](.github/workflows/release.yml) 定义六个 OS/arch 二进制、manifest、checksums.txt；只有**获得发布授权并实际存在 release 后**才使用以下通用安装步骤：

```bash
cloudpath plugin install <repository-url-or-id> --digest sha256:<binary-digest> --yes
cloudpath plugin enable io.github.deliciousbuding.cloud-path-app-environment-guard
```

当前组件尚未发布，以上不是一个已经可下载的 release 地址。本地集成由宿主维护者将构建产物注册到隔离 catalog，再通过 Application 实例 API 配置和检查真实绑定；本仓不修改 Core 或生产 catalog。

验收顺序：

1. 运行测试与 manifest gate，确认安装的 Core 符合 v0.2.15 观测/手动语义。
2. 检查实例 bindings 恰好对应 temperature/illuminance；默认 light_threshold 为 null。
3. 通过真实观测扇入查看 `GET /api/plugin-instances/{id}/records?record_type=environment`：值、单位、时间、质量与来源一致，光照没有未经校准的明暗结论。
4. 测温度上下限及恢复区间；光照数值测试显式启用阈值/方向，验证进入和恢复，不当作物理标定。
5. 保持稳定输入，确认不会每个采样新增历史；`alert` 不超过 8 个固定槽位。
6. 停止观测输入，等待 durable check-freshness 写 stale；读取 jobs/scheduled 及记录验证落库。点刷新不应改变采样时间。
7. 同时运行两个实例，检查各自记录、配置、latch、idempotency key 和效果流；其他应用的控制输出不受本插件影响。

## 真实边界

- 没有读取历史记录来重建采样缓存的 public SDK 接口；进程重启后先写 unknown，等新观测。固定 alert 槽位限制跨重启行数，但不保证跨重启零重复 upsert。
- 观测 sequence 在当前绑定生命周期内应单调；来源重启重置序号时需重启该应用实例或真正换绑；同实体重复 ValidateBinding 或仅重连事件流不会清空观测序号。已使用 sequence 后突然省略它会标记 unknown。Core 是否保留/恢复观测序号属于集成契约。
- 无时间、无 sequence 的来源只能按实际到达检查新鲜度，应用无法识别上游重新包装的旧值。timestamp/quality 必须尽量由可信链路提供。
- SDK 的效果发送没有数据库 ACK；本地 Send 成功不是 durable 落库或板测证明，断流期间中间变化只保留各槽位最后待发送记录。
- 原始 ADC 方向/温度量纲、Core 扇入、手动按钮、浏览器投影和真机验收不在本组件本地测试所能证明的范围内。
