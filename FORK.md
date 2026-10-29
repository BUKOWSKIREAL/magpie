# 此 fork 的构建与更新

此 fork 供 macOS 使用，自动发布 Apple Silicon（arm64）版本。上游的跨平台源码和 Go 模块声明保留，便于同步；本仓库不提供 Windows、Linux、Android 或 Docker 的构建发布入口。

## 构建

在 macOS 上安装 Xcode 命令行工具及 `go.mod` 指定的 Go 版本，然后运行：

```sh
make app          # 当前 Mac 架构的 magpie.app
make release-cli  # macOS arm64 的终端版本
```

自动发布使用 `.github/workflows/fork-release.yml`，生成 `magpie-darwin-arm64.zip`、`magpie-cli-darwin-arm64` 和 `latest.json`。它每四小时检查一次上游，也可以从 Actions 手动运行。

同步时将本 fork 的 Antigravity Desktop/CLI 会话用量补丁和更新源配置重新应用到上游 `main`。构建前运行 `internal/sessions` 和 `internal/usage` 的完整测试，包括响应 ID 去重回归测试。上游改动若与本地补丁冲突，同步会停止，需处理冲突后再运行；不会自动丢弃补丁。

`.github/workflows/test.yml` 仅使用 macOS runner。`privacy-filter.yml` 仅构建 macOS arm64 的本地隐私过滤组件，`native/privacy-filter/build.sh` 保留 macOS arm64/amd64 构建支持。原先禁用的辅助工作流保持禁用，自动更新发布由 `fork-release.yml` 负责。

上游文档中的其他平台安装和发布说明不适用于此 fork 的构建产物。
