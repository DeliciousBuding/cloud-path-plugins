# 组件边界

- 本仓只实现设备无关的 Sensor Alert Application；不改 CloudPath Core、Driver 或其他 Application。
- 仅依赖发布版 Core public SDK；正式 `go.mod` 不留本地 `replace`。
- 只消费绑定实体的 property-observed / CapabilityEvent，只发领域记录与通用 `tone` / `led` 命令。
- 禁止访问 COM3、启动/停止 Edge、烧录或修改现网配置；测试必须使用 fake event/effect writer。
- 验证：`go test ./... -count=1`、`go test -race ./... -count=1`、`go vet ./...`、`go build ./...`、`scripts/validate_manifest.py`。
- manifest、descriptor、requirements 必须一致；不提交构建产物、凭据或私有验收日志。
