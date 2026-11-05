# 更新日志

只记录**会改变默认行为**的变更；其余修复见提交记录与 Release 说明。

## 未发布

- 入群模式下题目发不出去时，`failure_policy: deny` 不再永久封禁新成员，改为封禁
  `failure_ban_seconds` 秒（新增配置，默认 600）后由 Telegram 自动解封，且不撤回对方
  的历史消息。解封后需要本人重新入群。
- 入群模式下出不了题（题库为空/损坏、图片取不到）时，`failure_policy: deny` 以前会让
  新成员不经验证留在群里；现在同样封禁 `failure_ban_seconds` 秒。`allow` 行为不变（放行）。
