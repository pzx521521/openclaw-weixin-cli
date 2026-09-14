# send：单次发件

发前做一次短轮询（刷新 `get_updates_buf` 和回信 `token`），然后给默认单 peer 发一条文本，`stdout` 只打一行 JSON。

## 用法

```bash
go run ./cmd/send [-state session.json] [-poll 2s] <text>
```

| 参数     | 默认           | 说明                               |
| -------- | -------------- | ---------------------------------- |
| `-state` | `session.json` | 会话文件（需先跑 `cmd/demo` 登录） |
| `-poll`  | `2s`           | 发前短轮询时长，只取历史不等待     |

目标选择：只有一个已知用户就用它，否则用当前选中的 peer；都没有则 `sent:false`。

## 返回（stdout 单行 JSON，exit 0）

成功：

```json
{"to":"xxx@im.wechat","msgs":[{"from":"xxx@im.wechat","text":"hi","time_ms":1726...}],"sent":true}
```

失败（`msgs` 照常返回累计消息）：

```json
{"msgs":[],"sent":false,"error":"no peer yet, wait for an inbound message"}
```

`msgs` = 自上次 `get_updates_buf` 以来积压的消息；`poll` 超时会被当空结果，照常用缓存 `token` 发送。

## 示例

```bash
go run ./cmd/send -state session.json "hello"
go run ./cmd/send -state session.1.json -poll 500ms "hi" | python3 -m json.tool
```
