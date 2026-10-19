# 加一个新网盘驱动

整个过程 15 分钟。以「假设你要接一个叫 `mysite` 的网盘」为例。

---

## 1. 建包

```
internal/storage/mysite/
└── mysite.go
```

## 2. 最小可用实现

复制下面这份骨架，改 `Type()`、`Init()`、`List()`、`Stat()` 四处就能跑起来：

```go
package mysite

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"panhub/internal/storage"
)

func init() {
	storage.Register("mysite", func() storage.Driver { return &Driver{} })
	storage.RegisterMeta("mysite", "我的网盘", []storage.Field{
		{Key: "token", Label: "访问令牌", Type: "password", Required: true,
			Help: "在网盘设置页生成"},
		{Key: "root_id", Label: "根目录 ID", Type: "text", Placeholder: "0"},
	})
}

type Driver struct {
	token  string
	rootID string
	client *http.Client
}

func (d *Driver) Type() string { return "mysite" }

// Caps 决定前端按钮亮不亮。
func (d *Driver) Caps() storage.Caps {
	return storage.Caps{Read: true} // 只读
	// 读写齐全就写：{Read: true, Write: true, Move: true, Copy: true, Upload: true, Link: true}
}

func (d *Driver) Init(_ context.Context, cfg map[string]any) error {
	d.token = storage.String(cfg, "token", "")
	if d.token == "" {
		return errors.New("mysite: 缺少 token")
	}
	d.rootID = storage.String(cfg, "root_id", "0")
	d.client = &http.Client{Timeout: 60 * time.Second}
	return nil
}

// List 返回 dir 下的直接子项。Path 用 storage.Join(dir, name) 拼接。
func (d *Driver) List(ctx context.Context, dir string) ([]storage.Entry, error) {
	// TODO: 调你自己的接口
	return nil, fmt.Errorf("mysite: 尚未实现: %w", storage.ErrNotImplemented)
}

// Stat 返回单个路径的信息。
func (d *Driver) Stat(ctx context.Context, p string) (storage.Entry, error) {
	parent := storage.Parent(p)
	name := storage.Base(p)
	es, err := d.List(ctx, parent)
	if err != nil {
		return storage.Entry{}, err
	}
	for _, e := range es {
		if e.Name == name {
			return e, nil
		}
	}
	return storage.Entry{}, storage.ErrNotFound
}
```

## 3. 注册到 main

`cmd/panhub/main.go` 的导入块加一行：

```go
_ "panhub/internal/storage/mysite"
```

## 4. 编译并验证

```bash
go build ./...
```

重启服务 → 「挂载管理 → 新建挂载」里就会出现「我的网盘」，表单字段由 `RegisterMeta` 自动生成。

---

## 5. 需要写能力时

按需实现这些**可选接口**（不用一次全做，实现一个就多一个能力）：

| 接口 | 方法 | 解锁的功能 |
|---|---|---|
| `storage.Writer` | `Mkdir` / `Remove` / `Upload` | 新建目录、删除、上传 |
| `storage.Mover` | `Move` | 重命名 / 移动 |
| `storage.Copier` | `Copy` | 服务端复制 |
| `storage.Opener` | `Open` | 代理播放 / 下载 |
| `storage.RangeOpener` | `OpenRange(p, offset)` | 播放器拖进度（**强烈建议实现**） |
| `storage.Linker` | `Link` | 302 直链，STRM 里写网盘地址，省服务器流量 |

### `OpenRange` 的重要性

Emby 拖进度会发 `Range: bytes=N-`。如果你的网盘支持 HTTP Range，直接带上请求头：

```go
func (d *Driver) OpenRange(ctx context.Context, p string, offset int64) (io.ReadCloser, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", d.downloadURL(p), nil)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, fmt.Errorf("mysite: HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}
```

如果不实现 `RangeOpener`，VFS 会退化成「重新打开后丢弃 N 字节」，能用但很慢。

---

## 6. 错误约定

返回这些哨兵错误，HTTP 层会自动映射状态码：

| 错误 | HTTP |
|---|---|
| `storage.ErrNotFound` | 404 |
| `storage.ErrNotSupported` | 400 |
| `storage.ErrNotConfigured` / `storage.ErrNotImplemented` | 501 |

其它错误一律 500，并且消息会原样返回给前端 —— **别把 token 拼进错误消息**。

---

## 7. 路径模型

- 驱动内路径统一是 `"/"` 开头的绝对路径，根是 `"/"`
- 用 `storage.Join(dir, name)`、`storage.Parent(p)`、`storage.Base(p)`、`storage.Clean(p)`
- **不要**自己拼 `path.Join` 之外的分隔符；Windows 路径只在 `local` 驱动里用 `filepath` 转换
- VFS 会自动给返回的 `Entry.Path` 补上挂载名前缀，**驱动里不要自己加**

---

## 8. 测试建议

在 `internal/storage/mysite/mysite_test.go` 里放一个能跑的假服务器：

```go
func TestList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"list":[{"name":"a.mkv","size":123,"dir":false}]}`)
	}))
	defer srv.Close()

	d := &Driver{}
	if err := d.Init(context.Background(), map[string]any{"token": "x", "base_url": srv.URL}); err != nil {
		t.Fatal(err)
	}
	es, err := d.List(context.Background(), "/")
	...
}
```

跑：`go test ./internal/storage/mysite/`

---

## 10. 让驱动支持登录（可选）

如果网盘需要先换 token/Cookie，实现 `storage.Authenticator` 即可，前端会自动多出登录面板：

```go
func (d *Driver) Login(ctx context.Context, req storage.LoginRequest) (*storage.LoginResult, error) {
	var auth string
	switch req.Mode {
	case "password":
		a, _, err := doPasswordLogin(ctx, req.Get("account"), req.Get("password"))
		if err != nil {
			return nil, err
		}
		auth = a
	case "authorization":
		auth = req.Get("authorization")
	default:
		return nil, fmt.Errorf("不支持的登录方式: %s", req.Mode)
	}
	return &storage.LoginResult{
		Session: map[string]any{"authorization": auth},          // 会合并进挂载配置
		Profile: map[string]any{"account": parseAccount(auth)},  // 仅用于展示
	}, nil
}
```

再声明登录方式（放在 `RegisterMeta` 之后）：

```go
storage.RegisterLogin("mysite", []string{"password", "authorization"}, map[string][]storage.LoginField{
	"password": {
		{Key: "account", Label: "账号", Type: "text", Required: true},
		{Key: "password", Label: "密码", Type: "password", Required: true},
	},
	"authorization": {
		{Key: "authorization", Label: "授权串", Type: "password", Required: true},
	},
})
```

约定：

- `Session` 里的键**必须**是挂载配置的键名（例如 `authorization`），前端直接 `Object.assign` 进表单
- 凭证失效时返回 `fmt.Errorf("...: %w", storage.ErrLoginRequired)`，HTTP 层会映射成 401
- 登录里做的网络请求要有超时；服务端会等 120 秒

---

## 11. 提交前的检查清单

- [ ] `Caps()` 与实际能力一致（不然前端会让你点一个必然失败的按钮）
- [ ] 只读网盘不要实现 `Writer`，或者在 UI 里勾「只读挂载」
- [ ] `List` 里跳过取不到元信息的条目，而不是整页失败
- [ ] 大文件走流式，不要 `io.ReadAll`
- [ ] 错误信息里不含密钥
- [ ] `go vet ./...` 干净
