# 组件边界

- 本仓只实现设备无关的 Environment Guard Application；不改平台、驱动或其他应用。
- 仅依赖发布版 Core public SDK；正式 go.mod 不留本地 replace。
- 只消费绑定温度/光照观测；只发领域记录与 durable schedule，不发设备动作或通知。
- 观测契约、配置、实例隔离、验收与真实限制见 README.md。
- 验证：go test ./...、go vet ./...、go build ./...、scripts/validate_manifest.py。
- manifest/descriptor/requirements 必须一致；版本与 release 资产保持一致。
- 不提交构建产物、临时 SDK 替换、凭据或私有验收日志；不自动发布或部署。
