# CloudPath Plugins

[![CI](https://github.com/DeliciousBuding/cloud-path-plugins/actions/workflows/ci.yml/badge.svg)](https://github.com/DeliciousBuding/cloud-path-plugins/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

CloudPath 官方 Driver 与 Application 插件源码单仓。每个插件仍保留独立模块、Manifest、版本号和 Release 资产；根目录 `plugins.yaml` 是 monorepo 的插件目录与发现索引。

## 现役插件

| Slug | 类型 | 版本 | 源码 | Release tag |
|---|---|---:|---|---|
| `stcb` | Driver | 0.2.14 | `drivers/stcb/` | `drivers/stcb/v0.2.14` |
| `scheduled-compartment` | Application | 0.3.3 | `apps/scheduled-compartment/` | `apps/scheduled-compartment/v0.3.3` |
| `button-indicator` | Application | 0.2.3 | `apps/button-indicator/` | `apps/button-indicator/v0.2.3` |
| `music-player` | Application | 0.3.3 | `apps/music-player/` | `apps/music-player/v0.3.3` |
| `sensor-alert` | Application | 0.2.3 | `apps/sensor-alert/` | `apps/sensor-alert/v0.2.3` |

## 安装

Core `v0.2.43+` 支持 monorepo catalog；指定插件 slug/id/path：

```bash
cloudpath plugin install DeliciousBuding/cloud-path-plugins \
  --plugin scheduled-compartment \
  --digest sha256:<release-digest> \
  --yes
```

单插件仓库的安装方式保持兼容。安装时应通过 `--digest`、Registry 或 attestation 提供独立信任证据，不应仅依赖同源 checksum。

## 开发

```bash
go work sync

cd drivers/stcb && go test ./... && go vet ./...
cd ../../apps/scheduled-compartment && go test ./...
```

根目录 catalog 与发布脚本不依赖第三方 Python 包：

```bash
python scripts/catalog.py --check
python scripts/catalog.py --list
python scripts/release.py --tag apps/music-player/v0.3.3 --out dist
```

## Release

插件独立 semver，tag 使用 `<module-path>/v<semver>`：

```text
drivers/stcb/v0.2.14
apps/scheduled-compartment/v0.3.3
apps/button-indicator/v0.2.3
apps/music-player/v0.3.3
apps/sensor-alert/v0.2.3
```

Release 工作流会校验 tag、`plugin.yaml`、catalog 和模块路径一致，再为六个平台构建二进制、生成 checksums，并附带 `LICENSE`、`NOTICE`、`THIRD_PARTY_NOTICES.md` 与 `plugin.yaml`。

## 归档

- `archive/hall-pillbox/`：药盒能力已并入 scheduled-compartment。
- `archive/environment-guard/`：监测能力已并入 sensor-alert。

归档源码不进入 catalog，不参与 CI/Release。

## 许可证

第一方代码采用 [Apache License 2.0](LICENSE)。第三方依赖和归档历史依赖的归属与完整许可证文本见 [NOTICE](NOTICE) 与 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
