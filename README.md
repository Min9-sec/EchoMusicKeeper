# EchoMusicKeeper

EchoMusicKeeper 是面向 Windows 弱网络环境的 EchoMusic 插件：普通酷狗歌曲在播放时可缓存到本地，也可保存为独立的永久下载文件。

## 安装

插件发布到 GitHub 后，在 EchoMusic 的“插件管理”中添加插件源：

```text
https://github.com/Min9-sec/EchoMusicKeeper
```

随后在插件列表中安装并启用 EchoMusicKeeper。在线安装只会提取仓库中的 `plugin` 运行目录，不会把源码和测试复制到本地插件目录。

也可以下载 GitHub Release 中的 Windows x64 ZIP 并解压到本地插件目录，或在“插件管理”中打开本地插件目录，将本仓库的 `plugin` 目录复制为 `echo-music-keeper` 文件夹后启用。

## 要求与首次运行

- 仅支持 Windows x64。
- 需要 EchoMusic `>=2.2.9-beta.29 <3`。
- 首次启用时，EchoMusic 会请求启动插件随附的 `bin/echo-music-keeper-helper.exe`。请在确认该文件来自可信插件包后允许执行；拒绝、启动失败或健康检查失败时，插件会回退到 EchoMusic 内置播放，不会阻止正常播放。

## 缓存范围和限制

自动缓存只处理普通酷狗歌曲，且仅处理 `effect: none` 的原始播放请求。云盘、非酷狗来源、音效请求和无法解析的歌曲继续交由 EchoMusic 的内置播放链路处理。

默认缓存目录是插件目录下的 `cache`，缓存上限为 1 GiB。空间达到限制时使用 LRU 清理，目标回收到上限的 90%。在“设置 - EchoMusicKeeper”中可直接输入缓存容量或选择自定义缓存目录；页面会显示当前实际使用的缓存和下载目录。

默认缓存目录会在插件更新、重装或卸载时清理。自定义缓存目录不会自动清理；已保存的下载文件也不会因更新、重装或卸载而删除。

## 下载和管理

永久下载默认保存到当前 Windows 用户的“音乐”目录。可在“设置 - EchoMusicKeeper”中选择其他下载目录；自定义下载目录中的文件同样会保留。

在歌曲的右键菜单中选择下载即可将歌曲加入下载队列。侧边栏“缓存与下载”页面可查看缓存占用、缓存条目和下载任务，并可清空缓存、删除某个永久下载文件或在资源管理器中显示下载目录。

清空缓存只删除缓存内容，不会删除永久下载。要删除永久下载，请在“缓存与下载”页面的下载列表中使用删除操作；也可以在资源管理器中自行删除对应文件。

## 弱网说明

缓存只能改善已经写入本地的片段和后续回放。网络的平均吞吐量低于所选音频码率时，首次播放仍可能卡顿或无法持续下载；此时可降低音质、等待缓存积累，或使用永久下载。

## 从源码构建

在仓库根目录执行：

```bash
npm ci
npm test
npm run build
cd helper
go test ./...
./build.ps1
```

`build.ps1` 需要 PowerShell、Go 1.25.4，并生成 Windows x64 的 `plugin/bin/echo-music-keeper-helper.exe`。`npm run check` 会验证 `plugin` 目录只包含安装所需文件。

## 发布

推送与 `package.json` 和 `plugin/manifest.json` 版本一致的 `vX.Y.Z` Tag 后，GitHub Actions 会运行完整检查并创建正式 Release。Release 包根目录可直接作为 EchoMusic 插件目录使用，同时附带 SHA-256 校验文件。

## 致谢

致敬并感谢 [EchoMusic](https://github.com/hoowhoami/EchoMusic) 项目。本插件基于 EchoMusic 提供的插件能力构建。
