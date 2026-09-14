# cloud：微信发件系统（多用户）

用户注册/登录后通过网页发微信消息。后端入口在此，前端为 `web/dist` 单页（登录/注册三步/发件）。

## 启动

```bash
export PGSTORE_WECHAT_DSN="postgres://user:pass@host:5432/db?sslmode=disable"
go run ./cmd/cloud [-port 7860] [-web web/dist] [-poll 2s] [-dsn ...]
```

| 配置                 | 默认       | 说明                        |
| -------------------- | ---------- | --------------------------- |
| `-port` ＞ `PORT`    | `7860`     | 监听端口，传参优先          |
| `-dsn`/`PGSTORE_WECHAT_DSN` | —   | PG 连接串，缺失直接退出     |
| `-web`               | `web/dist` | 前端静态目录                |
| `-poll`              | `2s`       | send/ready 的短轮询时长     |

表结构启动时自动迁移（`wechat_users`、`wechat_sessions`），`bot_token` 明文存库。

## 注册流程（网页）

1. 设密码（≥6 位）→ `POST /api/register/qr` 返回二维码 → 微信扫码。
2. 前端轮询 `GET /api/register/status?qrcode=` → `confirmed` 返回 `bot_id`。
3. `POST /api/register/finish`（后端先验 token 有效再落库，`bot_id` 重复报 409）→ 自动登录。
4. 用微信发任意一条消息 → 轮询 `GET /api/account/ready` 变 `true` → 可发件。

## API

| 方法   | 路径                    | 鉴权              | 说明                                    |
| ------ | ----------------------- | ----------------- | --------------------------------------- |
| `GET`  | `/health`               | 无                | 存活检查（含 DB ping），`{"ok":true}`   |
| `POST` | `/api/register/qr`      | 无                | `{password}` → `{qrcode, qr_png, expires_in}` |
| `GET`  | `/api/register/status`  | 无                | `?qrcode=` → `{status, bot_id?, ...}`   |
| `POST` | `/api/register/finish`  | 无                | `{password, bot_token, bot_id, user_id, base_url}` |
| `POST` | `/api/login`            | 无                | `{bot_id, password}` → 种 cookie        |
| `POST` | `/api/logout`           | cookie            | 清会话                                  |
| `GET`  | `/api/me`               | cookie            | `{bot_id, ready, peer}`                 |
| `GET`  | `/api/account/ready`    | cookie            | 短 poll 一次，`{ready, peer?}`          |
| `POST` | `/api/send`             | cookie **或** body | `{text}` 或 `{bot_id, password, text}` → `{to, msgs, sent, error?}` |

`send` 直调示例（脚本免登录）：

```bash
curl -s -X POST localhost:7860/api/send \
  -H 'Content-Type: application/json' \
  -d '{"bot_id":"xxx@im.bot","password":"***","text":"hi"}'
```

## Docker

```bash
docker build -t wechat-cloud .
docker run -p 7860:7860 -e PGSTORE_WECHAT_DSN="..." wechat-cloud
```

注意：密码每次进 body，生产环境必须 HTTPS（前置反代配证书或仅内网调用）。
