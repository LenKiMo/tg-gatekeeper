# tg-gatekeeper

> 一个通用的 Telegram 入群验证机器人：给新人发一张图片，附上若干候选答案，只有选对与图片对应的那一个才放行；选错、超时，或还没通过验证就发言，就请出去。

[![CI](https://github.com/LenKiMo/tg-gatekeeper/actions/workflows/ci.yml/badge.svg)](https://github.com/LenKiMo/tg-gatekeeper/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License: GPL-3.0](https://img.shields.io/badge/License-GPL--3.0-blue.svg)](./LICENSE)
[![Tests](https://img.shields.io/badge/tests-69%20passing-brightgreen)](#-开发与测试)
[![Deps](https://img.shields.io/badge/runtime%20deps-SQLite%20only-lightgrey)](#-架构)
[![AI](https://img.shields.io/badge/AIGC-AI--assisted-blueviolet)](#-aigc-声明)

题库就是一份「图片 → 答案」的对应关系表，换成你自己的素材，它验证的就是你自己的内容。
答案与干扰选项都来自同一份题库，所以每个选项看起来都同样合理。

---

## 📖 目录

- [✨ 功能](#-功能)
- [🚀 快速开始](#-快速开始)
- [📋 命令](#-命令)
- [⚙️ 配置](#-配置)
- [📚 自备题库](#-自备题库本仓库不含素材)
- [🏗 架构](#-架构)
- [🔒 安全与并发设计](#-安全与并发设计)
- [🐳 部署](#-部署)
- [🧪 开发与测试](#-开发与测试)
- [🙏 参考与致谢](#-参考与致谢)
- [⚖️ 许可证](#️-许可证)
- [🤖 AIGC 声明](#-aigc-声明)

---

## ✨ 功能

1. 有人入群（或提交入群申请）时，从题库里抽一条，把那一条的图片发出来；
2. 同时给出 `option_count` 个按钮（默认 12 个），其中**只有一个**与图片对应，其余都取自同一题库池（所以干扰项也都是真实存在的名字）；
3. 用户在 `timeout_seconds`（默认 60 秒）内选对 → 放行；选错、超时、或**还没验证就发言** → 移出群组（可在 `kick_unban_after_seconds` 后自动解封，允许重新尝试）；
4. 申请审批模式下，题目通过私聊发给申请者，同时群里发一张管理员处置卡片，管理员可一键放行或封禁；
5. 昵称/简介命中广告词 → 直接拦截；全过程写结构化审计日志。

| 模式 | 触发 | 出题位置 | 验证失败 |
| --- | --- | --- | --- |
| `join` | 有人入群 | 群内（先禁言） | 踢出，N 秒后自动解封 |
| `request` | 有人提交入群申请 | 申请者私聊 | 拒绝申请 |

其它已经能用的东西：

- **题库热更新**：`/reload` 重新加载题库，换题不必重启；数据集版本会一起变，避免抽到脏数据。
- **答题记录**：谁在什么时候答对/答错/超时，都进结构化审计日志（JSONL + 轮转 + 敏感字段脱敏）。
- **欢迎语与群规**：`/welcome` 设置欢迎语（支持 `{mention}`），`/reg` 回复一条消息即设为群规。
- **成员标签**：验证通过后自动设置群标签（`/tag`）。
- **两种题库来源**：本地文件/目录，或远程 HTTP（带 ETag 与缓存，题库可以放在你自己的服务上）。

## 🚀 快速开始

```bash
# 需要 Go 1.26+（见 go.mod）。仓库自带一份 12 条的最小示例题库（图片内嵌为 data URL），克隆后即可跑通。
export TG_BOT_TOKEN='123456:ABC...'          # 从 @BotFather 拿，别写进配置文件

# 1) 构建
go build -o tg-gatekeeper ./cmd/tg-gatekeeper

# 2) 配置 + 自检（配置 / 存储 / 题库 / 抽题 / 图片管线）
cp configs/gatekeeper.example.yaml configs/gatekeeper.yaml
./tg-gatekeeper check-config -c configs/gatekeeper.yaml

# 3) 启动（机器人需要是群管理员）
./tg-gatekeeper run -c configs/gatekeeper.yaml
```

给机器人的**最小权限**：`删除消息` + `禁止成员`（=禁言/踢出）。若要用申请审批模式再加 `邀请用户`
（Telegram 用它来投递入群申请）；`/tag` 功能另需 `管理标签`。

## 📋 命令

| 命令 | 权限 | 说明 |
| --- | --- | --- |
| `/ping` | 所有人 | 存活检查，附带题库条数与版本 |
| `/help` | 所有人 | 使用说明 |
| `/request_mode [join\|request]` | 管理员 | 查看/切换验证模式（不带参数则切换） |
| `/welcome <文本\|clear>` | 管理员 | 设置欢迎语，支持 `{mention}` 提及新成员 |
| `/reg` | 管理员 | 回复一条消息，把它设为群规（验证通过后随欢迎语给出） |
| `/tag <文本\|clear>` | 管理员 | 验证通过后自动设置的成员标签（≤16 字符） |
| `/reload` | `bot.owner_id` | 重新加载题库 |

## ⚙️ 配置

YAML，**未知字段直接报错**（避免拼错后静默用默认值），支持 `${ENV_NAME}` 引用环境变量；
全字段注释见 [`configs/gatekeeper.example.yaml`](./configs/gatekeeper.example.yaml)。最关键的几项：

```yaml
provider:
  active: main                  # 指向 provider.definitions 里的键
  definitions:
    main:
      type: jsonfile            # jsonfile | directory | sql | http
      jsonfile:
        path: ./data/items.json

gatekeeper:
  option_count: 12              # 选项个数（含正确答案）
  timeout_seconds: 60
  shortage_policy: deny         # deny=题库不足时不放行；reduce=降级到 min_option_count
  failure_policy: deny          # 基础设施故障时 deny（拒绝）/ allow（放行，会告警）
  block_pending_messages: true  # 未通过验证就发言 → 立即处置
```

## 📚 自备题库（本仓库不含素材）

题库格式：

```json
{"items": [{"id": "stable-id", "label": "正确标签", "image": "images/a.jpg",
            "pool": "default", "groups": ["可选分组"], "difficulty": 1,
            "weight": 1, "enabled": true, "alias": ["同义写法"]}]}
```

- `label`：该图片的正确答案（按钮文本）；`image`：`http(s)` URL / 相对路径（用 `base_dir` 解析）/ `file:` / `data:` base64
- `pool`：选项池，**干扰项只在同一 pool 内抽取**，用于把不同题材/分类分开
- `alias`：同一答案的其它写法（不会出现在选项里，避免"选它其实也对"）
- `difficulty` / `groups`：供 `same_group`、`difficulty_band` 抽题策略使用；`weight` 可压低冷门条目出现率

三种接入方式：

```bash
# ① 任意 JSON → 标准格式（自动识别 label/name/cnname 与 image/illustration/cover/url 等字段别名）
./tg-gatekeeper dataset json --input raw.json --out data/items.json --pool default

# ② MediaWiki 分类 → 标准格式（可选顺带下载并规范化图片：重编码去元信息、按稳定 ID 命名）
./tg-gatekeeper dataset wiki --api https://<wiki-host>/api.php \
  --category Category:<分类> --label-field cnname --image-field image \
  --thumb-width 600 --download-images data/images --out data/items.json

# ③ 直接把图片丢进目录，文件名就是答案（零转换）
#    data/images/<答案>.jpg  + 可选 data/labels.txt 做"文件名 → 答案"映射
```

`check-config` 会验证：图片与答案是否一一对应、每张图能否解码与重编码、能否组成 `option_count`
个互不重复的选项、正确答案是否确实在选项内。

⚠️ **素材许可由你负责**：本仓库不提供任何图片，也不对第三方素材的授权做任何保证。

## 🏗 架构

```
cmd/tg-gatekeeper        命令行入口：run / check-config / dataset
internal/domain          核心模型（Entry/Snapshot/GroupConfig/Session/Event/Effect），无框架依赖
internal/ports           全部外部依赖接口（Source/ImageResolver/Store/SessionRegistry/DeletionQueue/Dispatcher/Telegram/Audit）
internal/challenge       抽题：同池干扰项、按答案而非按图等概率、随机 token、题库不足策略
internal/provider        jsonfile / directory / sql / http 四种题库来源
internal/image           取图 + 限流限尺寸 + 重编码（抹掉 EXIF/文件名）+ 磁盘缓存 + SSRF 防护
internal/store           群配置（SQLite/MySQL/YAML）+ 会话注册表（原子 Claim）+ 延时删除队列（lease/ack/nack）
internal/dispatch        按 key 严格 FIFO、按 lane 限并发的分派器（关键通道有界背压不丢事件）
internal/gatekeeper      用例层：入群/申请/回调/未验证发言/超时扫描 + 副作用 outbox 执行器
internal/telegram        telegram-bot-api v1.1.1 适配器 + MarkdownV2 转义 + callback_data 编解码
internal/router          唯一更新入口：解包 → 提交队列（不做网络/数据库阻塞操作）
internal/app             依赖组装、启动顺序、优雅退出
internal/audit           结构化审计日志（JSONL + 轮转）
internal/filter          昵称/简介广告词过滤（NFKC / 零宽 / 分隔符归一化）
internal/dataset         题库导入（MediaWiki / 任意 JSON）与图片下载
```

依赖方向：`router/telegram → gatekeeper → domain + ports`，其余都是 ports 的实现。
运行时只需要一个本地 SQLite 文件（纯 Go 驱动，`CGO_ENABLED=0` 出单二进制），无需 MySQL/Redis。

## 🔒 安全与并发设计

- **正确答案只以 sha256 存在库里**：数据库/日志/按钮数据被看到也读不出答案；按钮里只有随机 token。
- **选项按"答案桶"等概率抽样**：图片多的条目不会更容易成为正确答案（有单元测试钉死）。
- **同池 + 别名排除**：干扰项都来自同一题库池，且"其实也对的别名"不会出现在选项里。
- **图片统一重编码**：输出文件名固定为 `challenge.jpg`，顺带抹掉 EXIF 与源文件名，防止"不看图、读元数据"。
- **图片 SSRF 防护**：默认拒绝内网/环回/链路本地地址，可配主机白名单；`file:` 引用被限制在 `local_roots` 内。
- **原子 Claim**：答题、超时、管理员操作、强行发言、退群在**同一个事务**里核对
  `(chat_id, user_id, session_id, state, expires_at)` 并写终态与副作用 outbox，因此并发点击只有一个能赢；
  超时之后即使答对也不会放行（只有 timeout 能赢）。
- **副作用 outbox**：终态与 Telegram 动作同事务落库，重启后继续执行；带重试退避与死信记录。
- **未验证发言拦截**：没通过验证就发言会被立刻处置（删消息 + 移出），该拦截挂在路由第一优先级。
- **分派器**：入群/申请/回调/发言走 critical 通道（有界背压，**不丢**）；命令走 command 通道（满载回"繁忙"）；
  同一用户/群严格 FIFO。

## 🐳 部署

```bash
cp configs/gatekeeper.example.yaml configs/gatekeeper.yaml   # 改 provider.active 指向你的题库
docker compose up -d
```

需要挂载的卷：

| 路径 | 用途 | 可写 |
| --- | --- | --- |
| `/app/configs` | 配置 | 只读即可（YAML 群配置模式需要可写） |
| `/app/data` | SQLite、题库、图片缓存、审计日志 | 是 |

镜像用 `golang:1.26` 构建、`debian:bookworm-slim` 运行，`CGO_ENABLED=0` 纯静态单二进制
（SQLite 使用纯 Go 驱动），也可以直接把二进制丢到任意 Linux/Windows/macOS 上跑。
容器以非 root 的 uid 10001 运行，所以挂载的 `data` 目录要先授权：`sudo chown -R 10001:10001 data`。

> 单实例约束：会话注册表与分派器都是进程内的，long-poll 也只能有一个消费者。
> 需要多副本请先改造成 webhook + 分布式租约，**不要只把 replicas 调大**。

## 🧪 开发与测试

```bash
go build ./...
go vet ./...
go test ./...                       # 69 个用例
go test ./internal/store/... -run TestClaimOnlyOneWinnerConcurrent -v   # 并发正确性
```

测试覆盖：抽题公平性与唯一性、别名排除、题库不足策略、并发 Claim（64 goroutine 只有一个赢）、
重启恢复、删除队列租约、分派器 FIFO 与背压、MarkdownV2 转义、callback 编解码、
广告词全角/零宽/分隔符绕过、图片路径穿越与 SSRF、12 个完整流程（入群/申请/答对/答错/超时/发言/管理员操作）。

## 🙏 参考与致谢

本项目的入群验证流程参考了以下开源工作，一并致谢：

- [IJNKAWAKAZE/arknights_bot](https://github.com/IJNKAWAKAZE/arknights_bot)（GPL-3.0）——入群验证与群管理的主要参考。
- [IJNKAWAKAZE/endfield_bot](https://github.com/IJNKAWAKAZE/endfield_bot)（GPL-3.0）——同源实现，一并参考。
- [ijnkawakaze/telegram-bot-api](https://github.com/ijnkawakaze/telegram-bot-api)（v1.1.1）——Telegram 客户端库。
- [modernc.org/sqlite](https://gitlab.com/cznic/sqlite)、[go-sql-driver/mysql](https://github.com/go-sql-driver/mysql)、
  [golang.org/x/text](https://pkg.go.dev/golang.org/x/text)、[golang.org/x/image](https://pkg.go.dev/golang.org/x/image)、
  [gopkg.in/yaml.v3](https://pkg.go.dev/gopkg.in/yaml.v3)。

## ⚖️ 许可证

本项目使用 [GPL-3.0](./LICENSE) 许可协议。

## 🤖 AIGC 声明

本仓库部分代码由 AI 辅助生成，已由人类维护者审阅、测试与验证。
