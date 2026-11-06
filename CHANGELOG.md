# 更新日志

只记录**会改变默认行为**的变更；其余修复见提交记录与 Release 说明。

## 未发布

- 入群模式下题目发不出去时，`failure_policy: deny` 不再永久封禁新成员，改为封禁
  `failure_ban_seconds` 秒（新增配置，默认 600）后由 Telegram 自动解封，且不撤回对方
  的历史消息。解封后需要本人重新入群。
- 入群模式下出不了题（题库为空/损坏、图片取不到）时，`failure_policy: deny` 以前会让
  新成员不经验证留在群里；现在同样封禁 `failure_ban_seconds` 秒。`allow` 行为不变（放行）。
- 申请审批模式下答错：以前是"封禁 + 稍后解封"且不处理申请，现在只拒绝申请、不封禁，
  并删除群里的管理员卡片（与 README 里"验证失败 → 拒绝申请"的说明一致）。
- 自动踢出（答错 / 超时 / 没验证就发言）改为一条定时封禁，`kick_unban_after_seconds` 秒后
  由 Telegram 自动解封，不再单独安排解封任务。默认仍是 60 秒；配 0 仍是永久；1..59 现在
  按 60 秒处理。
- **行为反转**：管理员在卡片上点"封禁"不再是永久封禁，改为封禁 `admin_ban_seconds` 秒
  （新增配置，默认 604800 = 7 天）；需要永久封禁请显式配成 0。
- 申请审批模式下命中广告词：默认仍然只拒绝申请（不改现有行为）；新增
  `ad_word_request_ban_days`（默认 0），配成 > 0 时先封禁该天数再拒绝，用于扛住重复申请冲击。
- 管理员并发改同一个群的配置时，后提交的人不再静默覆盖先提交的字段，而是收到
  "配置刚被其他管理员改过，请重新操作"。命令处理也改为按群串行。
- 机器人重启后，重启前发出的题目卡片（含管理员卡片上的放行/封禁键）仍然可点：
  回调不再按"题目消息的发送时间"被判为积压丢弃，点击是否有效由会话状态与超时决定。
- 本地题库的图片被原地替换后立刻生效（缓存键加入源图 mtime 与大小）；HTTP 图片在
  服务端提供 ETag / Last-Modified 时改为条件请求（304 复用、200 换新图）而不是等 TTL。
- `images.allowed_schemes` 现在按协议逐个校验：只写 https 时不再放行 http。
- `storage.yaml.runtime_writes=false` 时，遇到未配置的群不再写盘（以前任何一条群消息都会写）。
- `provider.definitions` 里的 `${ENV}` 现在会展开（以前 bearer_token / dsn 会保持字面值）。
- 副作用永久错误的判定改用 Telegram 结构化错误码（不再对 "400" 做文本匹配），
  日志与审计里的错误文本会先把 bot token 换成 `[token]`。
- `dataset --download-images` 为非 ASCII 标签生成可区分的文件名（以前全部落到同一个
  `.jpg` 上，并在重名时把别的图当成"已下载"）。
- **行为反转**：移除 MySQL 后端，只保留 SQLite。`storage.group_store` 收窄为 `sqlite|yaml`、
  `session_registry`/`deletion_queue` 收窄为 `sqlite|memory`、题库 SQL 源只支持 `driver: sqlite`；
  老配置里写 `mysql` 会在启动自检阶段直接报错（见 docs/mysql-backend-assessment.md）。
