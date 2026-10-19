# 第三方组件与出处说明

本项目自身以 **PolyForm Noncommercial License 1.0.0** 发布（见 `LICENSE`）。本文件登记
项目使用的外部代码、依赖与素材及其许可，供使用者核对。

## 一、源码引用

**截至 2026-10-19 审计：本仓库未复制、改写或内联任何第三方源码。** 仓库内没有 `vendor/`
目录，没有第三方字体、图标或图片素材；`web/dist` 为自建产物且不入库。

监控面板的功能与信息架构参照了 [哪吒监控（Nezha）](https://github.com/nezhahq/nezha)，
属**设计层面的参照**，未使用其代码或资源。若后续复制其任何代码片段，必须在下表登记：
来源仓库、commit、文件路径、原许可证，并在文件头保留原版权声明。

| 来源 | 用途 | 许可证 | 说明 |
| --- | --- | --- | --- |
| — | — | — | 目前无 |

## 二、Go 依赖（编译进 hub / agent 二进制）

| 模块 | 版本 | 许可证 |
| --- | --- | --- |
| `github.com/coder/websocket` | v1.8.15 | ISC |
| `golang.org/x/crypto` | v0.27.0 | BSD-3-Clause |
| `modernc.org/sqlite` | v1.34.1 | BSD-3-Clause |
| `github.com/dustin/go-humanize` | v1.0.1 | MIT |
| `github.com/google/uuid` | v1.6.0 | BSD-3-Clause |
| `github.com/hashicorp/golang-lru/v2` | v2.0.7 | MPL-2.0 |
| `github.com/mattn/go-isatty` | v0.0.20 | MIT |
| `github.com/ncruces/go-strftime` | v0.1.9 | MIT |
| `github.com/remyoudompheng/bigfft` | v0.0.0-20230129092748-24d4a6f8daec | BSD-3-Clause |
| `golang.org/x/sys` | v0.25.0 | BSD-3-Clause |
| `modernc.org/gc/v3` | v3.0.0-20240107210532-573471604cb6 | BSD-3-Clause |
| `modernc.org/libc` | v1.55.3 | BSD-3-Clause |
| `modernc.org/mathutil` | v1.6.0 | BSD-3-Clause |
| `modernc.org/memory` | v1.8.0 | BSD-3-Clause |
| `modernc.org/strutil` | v1.2.0 | BSD-3-Clause |
| `modernc.org/token` | v1.1.0 | BSD-3-Clause |

上述模块以 Go module 形式引入，未修改其源码；各模块的完整许可证原文随模块分发，
位于 `$GOMODCACHE/<模块>@<版本>/LICENSE`。间接依赖（`// indirect`）由上述模块带入。

## 三、前端运行时依赖（打包进 `web/dist`，内嵌进二进制）

| 包 | 版本 | 许可证 |
| --- | --- | --- |
| `vue` | 3.5.44 | MIT |
| `vue-router` | 5.4.0 | MIT |
| `pinia` | 4.1.0 | MIT |
| `uplot` | 1.6.32 | MIT |

## 四、开发期依赖（不随产物分发）

`vite`、`typescript`、`vue-tsc`、`@vitejs/plugin-vue`、`@types/node` —— 仅构建与类型检查时使用，
不进入发布产物。

## 五、维护约定

1. 新增或升级依赖，**同一提交内**更新本文件；`python3 tools/check_third_party.py` 校验一致性。
2. 复制任何外部代码、片段、图标、字体、文案之前，先确认其许可证与本项目兼容（本项目为非商业
   源码许可），按「一、源码引用」登记出处与许可证，必要时在文件头保留原版权声明。
3. 来源不明或无法确认许可证的代码，**禁止进入本仓库**。
