# panhub

多网盘聚合挂载：把本地目录、WebDAV、云盘统一挂到一棵虚拟目录树下，用 **Web 界面浏览**、用 **WebDAV 给播放器/文件管理器用**、用 **STRM 喂给 Emby/Jellyfin**。

> 这是一个**独立重写**的实现，不是 [LitePan](https://github.com/Ponphil/LitePan) 的代码副本（LitePan 使用 PolyForm Noncommercial 许可、禁止商用）。功能思路上参考了它的方向，代码、结构和前端都是本仓库自己的。

---

## 现在能做什么

| 能力 | 状态 | 说明 |
|---|---|---|
| 驱动框架（可插拔） | ✅ | 加网盘 = 加一个子包，见 [docs/ADD_A_DRIVER.md](docs/ADD_A_DRIVER.md) |
| 本地目录驱动 | ✅ | 读写、移动、复制、上传、Range 播放 |
| WebDAV 客户端驱动 | ✅ | 手写 PROPFIND/MKCOL/MOVE/COPY/PUT/GET，可直接挂 Alist、Nextcloud |
| 天翼云盘（cloud189） | ⚠️ 实验性 | 用浏览器 Cookie 调内部接口，官方可能变；需自备 Cookie 联调 |
| 移动云盘（cloud139） | ✅ 可登录 | 三种登录方式（账号密码 / 授权 / Cookie）、签名、列目录、直链；接口为逆向实现，见下文 |
| Web 界面 | ✅ | 登录、文件浏览、挂载管理、任务管理 |
| 虚拟目录聚合 | ✅ | `/` 列出所有挂载点，`/挂载名/...` 访问 |
| WebDAV 服务端 | ✅ | `/dav`，支持 PROPFIND/GET/PUT/MKCOL/MOVE/COPY/DELETE |
| Range 断点/拖拽 | ✅ | Emby 拖进度、播放器 seek 都靠它 |
| STRM 生成 + 定时任务 | ✅ | 目录递归、只挑媒体后缀、内容不变不重写 |
| 直链（302） | ✅ 框架就绪 | 驱动实现 `Linker` 即可；本地/WebDAV 不支持，自动回落代理 |
| FUSE 本地挂载 | ❌ 未做 | 需要 CGO + /dev/fuse，后续可加 |
| 跨盘秒传 / 离线下载 / TMDB 刮削 | ❌ 未做 | 这些是另一个量级的功能 |

---

## 快速开始

### 方式一：源码运行

需要 Go 1.22+ 和 Node 18+。

```bash
# 1. 构建前端（产物会被 Go embed 进二进制）
cd web && npm install && npm run build && cd ..

# 2. 编译
go build -o panhub ./cmd/panhub

# 3. 准备配置
cp config.example.yaml config.yaml     # 按需改密码、挂载目录

# 4. 启动
./panhub -config config.yaml
```

打开 <http://127.0.0.1:5211>，默认密码 `admin`（**记得改**）。

### 方式二：Docker

> 目前**没有公开镜像可以 `docker pull`**，需要本地构建，或按下面的步骤发布到你自己的镜像仓库。

**本地构建 + 运行**：

```bash
docker compose up -d --build
# 或
docker build -t panhub:latest .
docker run -d --name panhub -p 5211:5211 \
  -v ./data:/app/data -v ./strm:/app/strm -v ./media:/media:ro \
  -e PANHUB_PASSWORD=admin \
  panhub:latest
```

镜像特点：前端已打包进二进制（单文件）、`amd64`/`arm64` 都能构建、带健康检查、无配置文件也能启动（默认 `:5211` / 密码 `admin`）。

**发布成可 `docker pull` 的镜像**（二选一）：

| 仓库 | 适合 | 步骤 |
|---|---|---|
| **GHCR**（GitHub） | 已把代码推到 GitHub | 什么都不用配：仓库已带 [.github/workflows/docker.yml](.github/workflows/docker.yml)，推到 `main` 或打 `v*` tag 就会自动构建 `amd64+arm64` 并推送 `ghcr.io/<你的用户名>/panhub:latest`（首次需在 GitHub → Packages 把镜像设为 public） |
| **Docker Hub** | 想用国内更快的拉取 | 1. 在 Docker Hub 建 `panhub` 仓库；2. 本地 `docker login`；3. `docker build -t <你的用户名>/panhub:latest .` → `docker push <你的用户名>/panhub:latest` |

发布后别人（或你的 NAS）只需：

```bash
docker pull ghcr.io/<你的用户名>/panhub:latest     # 或 <你的用户名>/panhub:latest
```

`docker-compose.yml` 里把 `image:` 换成真实地址、注释掉 `build: .` 即可。

注意事项：

- 容器里不知道自己的对外地址，**生成 STRM 前请设 `PANHUB_BASE_URL`**（例如 `http://192.168.1.10:5211`）
- 想让 Emby 读到 STRM，宿主机上的 `./strm` 要挂给 Emby 同一个路径
- 139 的 Cookie/密码等凭证存在 `data/state.json`（挂载卷里），备份它就能整体迁移

---

## 上手三步

### 1. 加一个挂载点

「挂载管理 → 新建挂载」，选驱动后表单字段会自动出来（字段由驱动的 `RegisterMeta` 声明）。

配置示例（`config.yaml`，首次启动会写入 `data/state.json`）：

```yaml
mounts:
  - name: media
    type: local
    read_only: false
    config:
      root: "D:/Media"
  - name: alist
    type: webdav
    read_only: true
    config:
      url: "http://192.168.1.10:5244/dav"
      username: admin
      password: xxx
```

### 2. 用 WebDAV 接入播放器

地址 `http://<你的IP>:5211/dav`，账号 `admin`，密码是配置里的密码。

适用：Infuse、Kodi（WebDAV 源）、Windows 资源管理器「映射网络驱动器」、RaiDrive、Documents 等。

路径规则：`/dav/<挂载名>/<目录>`，例如 `/dav/alist/movies`。

### 3. 让 Emby / Jellyfin 用 STRM

「STRM 任务 → 新建任务」：

- 挂载点：`alist`，源目录：`/movies`
- 输出目录：宿主机绝对路径（如 `D:/strm/movies`）
- 链接方式：`proxy`（走本服务，兼容性最好）
- 间隔：`6h`

任务会把网盘里的 `.mkv/.mp4/...` 生成同名 `.strm`，内容是带 `stream_key` 的播放地址。然后：

1. Emby 里添加媒体库，路径指向 `D:/strm/movies`
2. 库设置里打开「将 STRM 文件视为视频」
3. 完成。新片入库后跑一次任务即可

`stream_key` 持久化在 `data/state.json`，重启不会让已有 STRM 失效。

---

## 移动云盘（139）登录

「新建挂载 → 选中国移动云盘」后，表单顶部会多出一个**登录面板**，三种方式任选：

| 方式 | 填什么 | 适用 |
|---|---|---|
| 账号密码 | 手机号/邮箱 + 邮箱登录密码 | 最省事；但可能撞上邮箱风控 |
| 粘贴授权 | `Authorization` 里 `Basic ` 后面那串 | **最稳**，风控时用这个 |
| Cookie 登录 | `mail.10086.cn` 的 Cookie（需含 `Os_Sso_Sid`、`RMKEY`） | 已有浏览器会话时最快 |

登录成功后，`authorization`、`account`、`user_domain_id` 会自动填进挂载配置，点「创建」即可。

### 账号密码登录做了什么

1. `mail.10086.cn/Login/Login.ashx`：账号 + `sha1("fetion.com.cn:"+密码)` 登录邮箱 SSO，从 302 的 `Location` 或 Set-Cookie 取 `sid`
2. `smsrebuild1.mail.10086.cn/setting/s?func=umc:getArtifact`：用 `sid` + `RMKEY` 换 `artifact`
3. `user-njs.yun.139.com/user/thirdlogin`：AES-CBC(key1) 加密请求 → 响应里 `data` 是 hex(AES-ECB(key2, 内层 JSON))，取出 `authToken`

最终 `Authorization = base64("pc:" + account + ":" + authToken)`，快过期时调
`aas.caiyun.feixin.10086.cn/tellin/authTokenRefresh.do` 自动续期。

### 手动取 Authorization（兜底）

1. 浏览器登录 <https://yun.139.com>
2. F12 → Network → 随便点一个 `getDisk` / `hcy/file/list` 请求
3. 复制请求头 `Authorization: Basic xxxxx` 里 **Basic 后面** 的内容
4. 粘到「粘贴授权」里 → 登录

### `.cas` 秒传播放

`.cas` 是移动云盘的「秒传凭证」文件：内容是 `Base64(JSON)`，里面带原片的 `sha256`、大小和文件名。
播放它不需要真的下载或上传：

```
读 .cas → 解出 sha256/size
  → 在临时目录里 POST /file/create（contentHash=SHA256）
     云端发现源数据还在 → 直接实例化出真文件（秒传，不传字节）
  → /file/getDownloadUrl 取直链 → 302 给播放器
```

在挂载配置里可以调：

| 配置项 | 默认 | 说明 |
|---|---|---|
| `cas_enabled` | 开 | 关掉后 `.cas` 就当普通文件处理（返回它本身的下载地址） |
| `cas_ttl` | `5m` | 还原出来的临时文件保留多久；过期自动移入回收站 |
| `cas_temp_dir` | `PANHUB_CAS_TEMP` | 临时目录名（位于云盘根目录，自动创建） |

实现要点（都是踩过坑才知道的）：

- **`/file/create` 必须伪装成 PC 客户端**：`x-MM-Source: 000`、`x-yun-op-type: 1`、
  `x-yun-device-id`、`x-ExpRoute-Code: routeCode=<账号>,type=2`、channel `10200153`。
  用 web 那套头会被判「拒绝秒传」。
- `partInfos` 按 100MB 切片（>30GB 用 512MB），只发前 100 片。
- 同一个 `.cas` 在 TTL 内复用直链，不重复建文件；过期后调 `/recyclebin/batchTrash` 清理。
- **只认 SHA256**：`.cas` 里只有 md5/sha1 会明确报错（115 的 `.cas` 就是 sha1，不能直接用）。
- **账号权益限制**：秒传大文件需要会员等级支持，被拒时是云盘侧限制，程序绕不过去。

> 首次播放会有 2~5 秒的还原耗时（取 .cas → 秒传 → 取链），之后命中缓存就是直接 302。

### 会遇到的问题

- **`接口返回失败: 无接口权限,服务器拒绝执行`**：你的账号已迁到「新个人盘」，旧 `getDisk` 没权限。
  本驱动默认 `api_type=auto`：**先走新盘 `/file/list`，失败再回退旧盘**，一般不会碰到。
  若仍报错，把 `api_type` 显式设成 `personal_new` 再试。
- **`ec=S002` 之类**：账号密码不对，或触发邮箱风控。当前版本**没有实现短信验证码流程**，遇到风控请改用「粘贴授权」。
- **`授权失效（HTTP 401/403）`**：Authorization 过期，重新登录即可。
- **根目录是空的**：新盘根 ID 是 `/`、旧盘是 `root`。留空会按接口自动选；挂载子目录时填网页端的 `parentFileId`（新盘）或 `catalogID`（旧盘）。

### 两套接口的区别

| `api_type` | 接口 | 说明 |
|---|---|---|
| `auto`（默认） | 先 `/file/list` 后 `getDisk` | 自动探测并记住可用的一套 |
| `personal_new` | 先查 `/user/route/qryRoutePolicy` 拿专属 host，再 `/file/list`、`/file/getDownloadUrl` | 新账号用这套 |
| `personal` | `/orchestration/.../getDisk`、`downloadRequest` | 未迁移的老账号 |

`client_type` 决定上报的客户端标识（`web`→9 / `pc`→13），一般不用改。

> 验证程度：签名算法、三步加解密、续期、目录解析都有离线单测；第 1 步对真实端点做过联网探测（能拿到响应与风控码）。**完整登录成功分支需要真实账号跑一次**。

---

## 目录结构

```
panhub/
├── cmd/panhub/main.go            入口：装配驱动、VFS、任务、HTTP 服务
├── internal/
│   ├── storage/                  ★ 驱动契约（你要改的地方大多在这）
│   │   ├── driver.go             接口、哨兵错误、路径工具、注册表
│   │   ├── meta.go               驱动表单字段声明（前端自动渲染）
│   │   ├── local/                本地目录驱动
│   │   ├── webdav/               WebDAV 客户端驱动（手写协议）
│   │   ├── cloud189/             天翼云盘（实验性）
│   │   └── cloud139/             移动云盘（脚手架）
│   ├── vfs/                      多挂载聚合：/ 与 /挂载名/...
│   ├── server/                   HTTP 层
│   │   ├── routes.go             路由（注意 DAV 绕过了 chi）
│   │   ├── api.go                REST 接口
│   │   ├── auth.go               Token / Basic 认证
│   │   ├── dav.go                VFS → WebDAV 文件系统适配
│   │   └── webui.go? → ../webui  前端 embed
│   ├── webui/                    前端构建产物（go:embed）
│   ├── config/                   YAML + 环境变量
│   ├── store/                    state.json 持久化
│   ├── strm/                     .strm 生成规则
│   └── task/                     定时任务（STRM）
├── web/                          Vue3 + Vite 前端源码
├── scripts/smoke.ps1             端到端冒烟测试（21 项断言）
├── config.example.yaml
├── Dockerfile / docker-compose.yml
└── docs/ADD_A_DRIVER.md          加新网盘教程
```

---

## API 一览

除 `/api/auth/login`、`/api/stream` 外都需要 `Authorization: Bearer <token>`。

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/auth/login` | `{"password":"..."}` → `{"token":...}` |
| GET | `/api/status` | 版本、挂载数、base_url |
| GET | `/api/drivers` | 驱动列表 + 表单字段 + 登录方式 |
| POST | `/api/drivers/{type}/login` | 驱动登录：`{mode,fields}` → `{session,profile,message}` |
| GET/POST | `/api/mounts` | 列表 / 新建 |
| PUT/DELETE | `/api/mounts/{name}` | 更新 / 删除 |
| GET | `/api/fs/list?path=` | 列目录 |
| GET | `/api/fs/stat?path=` | 单文件信息 |
| GET | `/api/fs/download?path=&token=` | 下载（支持 Range） |
| POST | `/api/fs/upload?path=` | multipart 上传 |
| POST | `/api/fs/mkdir` `remove` `move` `copy` | 目录操作 |
| GET | `/api/stream?path=&key=` | 播放（无需登录，靠 stream_key） |
| GET/POST | `/api/tasks` | 任务列表 / 新建 |
| POST | `/api/tasks/{id}/run` | 立即执行 |

---

## 加一个新网盘

见 [docs/ADD_A_DRIVER.md](docs/ADD_A_DRIVER.md)。核心就三步：

1. 新建 `internal/storage/你的网盘/xxx.go`，实现 `Type/Caps/Init/List/Stat`
2. `init()` 里 `storage.Register` + `storage.RegisterMeta`
3. `cmd/panhub/main.go` 加一行空导入

写操作、直链、流式读取都是**可选接口**（`Writer`/`Linker`/`Opener`），实现哪个就自动多出哪个能力。

---

## 开发与测试

```bash
# 后端编译（不重建前端）
go build ./...

# 前端热重载（另一个终端先跑 go run ./cmd/panhub）
cd web && npm run dev      # http://127.0.0.1:5173，自动代理 /api 和 /dav

# 端到端测试（会自己起服务、建临时挂载、测 21 项）
pwsh -NoProfile -ExecutionPolicy Bypass -File scripts/smoke.ps1

# 调试日志
PANHUB_DEBUG=1 ./panhub -config config.yaml
```

---

## 已知限制

- **天翼云盘**：没有官方 API，用的是网页版 Cookie。Cookie 过期需重新填；接口字段变了要改 `cloud189.go`（端点都在文件顶部常量里）。
- **移动云盘**：`mcloud-sign` 签名算法未校准，目前只有鉴权字段和接口形状，`List` 会明确返回「未联调」。
- **只读建议**：对网盘挂载建议勾选只读，避免误删云端文件。
- **无跨挂载点复制**：`Move/Copy` 目前限定同一挂载点内。
- **WebDAV 写入缓冲在内存**：通过 DAV 上传大文件会占内存，API 上传是流式的。给播放器只读用没问题。
- **未做限速/多用户**：单管理员账号，公网暴露请套反代 + HTTPS。

---

## 安全提醒

- 默认密码 `admin` 必须改，`PANHUB_PASSWORD` 或 `config.yaml` 都行
- 公网部署务必放在 Nginx/Caddy 后面并启用 HTTPS
- 网盘 Cookie / 密码以明文存在 `data/state.json`（权限 0600），注意备份文件的流向
- `.strm` 里的 `stream_key` 等同于「无需登录的播放凭证」，泄露后对方可播放你所有挂载的媒体
