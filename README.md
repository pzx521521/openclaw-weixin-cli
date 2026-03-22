# 微信-openclaw CLI
这是一个独立的 Go 命令行程序，用来对接微信 iLink Bot HTTP API。

# 说明
微信新出了`openclaw-weixin-cli`用于和龙虾的交互,   
其实就是一个简单的 消息接收/回复,  
解析了一下里面的原理,并做了简单的实现.  
可以完全脱离 OpenClaw 使用，但是目前不知道能干什么...

## 功能
- 自动模式：如果 `session.json` 可用，直接进入聊天模式；否则自动进入扫码登录
- 扫码登录，并将会话信息持久化到本地
- 登录成功后立即进入聊天模式
- 使用 HTTP 长轮询 `getupdates` 接收消息
- 在终端里回复文本消息
- 将已知联系人持久化到 `session.json`，下次启动自动恢复
- 不依赖 OpenClaw 运行时

## 文件说明

- `main.go`：CLI 入口与交互式聊天循环
- `client.go`：微信 iLink HTTP API 客户端
- `store.go`：会话与联系人持久化
- `session.json`：登录后生成的会话文件
- `login-qr.png`：登录时生成的二维码图片

## 构建

```bash
go build -o wechat-cli .
```


行为如下：

- 如果 `session.json` 有效，则直接进入聊天模式
- 如果 `session.json` 不存在或不可用，则进入扫码登录
- 登录成功后，当前进程会立即切换到聊天模式

聊天模式支持以下命令：

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
