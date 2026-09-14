# 微信-openclaw CLI
这是一个独立的 Go 命令行程序，用来对接微信 iLink Bot HTTP API。

# 说明
> 原始代码[openclaw-weixin](https://github.com/Tencent/openclaw-weixin)  
> 把`openclaw-weixin-cli`单独拉出来做消息接收和回复

微信新出了`openclaw-weixin-cli`用于和龙虾的交互,   
其实就是一个简单的 消息接收/回复,  
解析了一下里面的原理,就是一个 http 长链接,并做了简单的实现.  
可以完全脱离 OpenClaw 使用，但是目前不知道能干什么...

## 原理速览

- 没有 WebSocket/回调，全靠 HTTP 长轮询 `POST /ilink/bot/getupdates` 收消息。
- `get_updates_buf` 是服务端的游标（断点续传）：首次传空，有新消息才变，发消息不影响它。
- 发消息另走 `POST /ilink/bot/sendmessage`，必填对端 `context_token`——它只能从对方先发来的消息里拿到，所以新好友必须先说第一句话。
- `bot_token` 长期有效，服务端回 `ret/errcode=-14` 才算过期，需重新扫码。

## 目录结构

- `cmd/demo/`：原交互式 CLI（扫码登录 + 长轮询聊天 + 终端回复），详见 `cmd/demo`（与原根目录功能一致）。
- `cmd/send/`：单次发件（短轮询后发送，stdout 单行 JSON），详见 `cmd/send/README.md`。
- `cmd/cloud/`：多用户发件系统（注册/登录/网页发件，PG 存储），详见 `cmd/cloud/README.md`。
- `internal/ilink/`：微信 iLink API 客户端 + 会话/联系人模型的共享包。
- `internal/cloud/`：发件系统后端源码。
- `web/dist/`：发件系统前端（无构建，原生单页）。
- `Dockerfile`：`cloud` 服务镜像。

## 构建

```bash
go build ./...
go run ./cmd/demo [-state session.json]   # 交互式聊天（自动登录或扫码）
go run ./cmd/send -state session.json "hi" # 单次发送
```

demo 聊天模式支持以下命令：

- `/help`：显示帮助
- `/users`：列出已知联系人
- `/who`：显示当前联系人
- `/use <peer>`：切换当前联系人
- `/send <peer> <message>`：向指定联系人发送消息
- `/quit`：退出聊天模式

直接输入普通文本，会发送给当前联系人。

注意：

- 只有收到过该用户消息并拿到 `context_token` 后，才能对这个用户回复
- 仅有登录账号自己的 `user_id` 不足以主动给任意用户发消息
- 一个 `session.json` 只对应一个扫码绑定的号，多号用 `-state` 分文件跑




社区   
[52pojie](https://www.52pojie.cn/)
