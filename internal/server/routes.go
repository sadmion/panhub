package server

import (
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/net/webdav"
)

// Handler 组装全部路由。
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(requestLogger)

	r.Route("/api", func(r chi.Router) {
		// 不需要登录的接口
		r.Post("/auth/login", s.login)
		r.Get("/stream", s.stream)
		r.Head("/stream", s.stream)

		// 需要登录的接口
		r.Group(func(r chi.Router) {
			r.Use(s.authRequired)
			r.Post("/auth/logout", s.logout)
			r.Get("/status", s.status)
			r.Get("/drivers", s.drivers)

			r.Get("/mounts", s.listMounts)
			r.Post("/mounts", s.createMount)
			r.Put("/mounts/{name}", s.updateMount)
			r.Delete("/mounts/{name}", s.deleteMount)

			// 驱动登录（账号密码 / Cookie / 粘贴授权）
			r.Post("/drivers/{type}/login", s.driverLogin)

			r.Get("/fs/list", s.fsList)
			r.Get("/fs/stat", s.fsStat)
			r.Get("/fs/link", s.fsLink)
			r.Get("/fs/download", s.fsDownload)
			r.Post("/fs/check-target", s.fsCheckTarget)
			r.Post("/fs/upload", s.fsUpload)
			r.Post("/fs/mkdir", s.fsMkdir)
			r.Post("/fs/remove", s.fsRemove)
			r.Post("/fs/move", s.fsMove)
			r.Post("/fs/copy", s.fsCopy)

			r.Get("/tasks", s.listTasks)
			r.Post("/tasks", s.createTask)
			r.Put("/tasks/{id}", s.updateTask)
			r.Delete("/tasks/{id}", s.deleteTask)
			r.Post("/tasks/{id}/run", s.runTask)
		})
	})

	// WebDAV：Kodi / Infuse / Windows 资源管理器 / RaiDrive 都走这里。
	davH := &webdav.Handler{
		Prefix:     "/dav",
		FileSystem: &davFS{v: s.v},
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				slog.Debug("webdav 失败", "method", r.Method, "path", r.URL.Path, "destination", r.Header.Get("Destination"), "err", err)
			}
		},
	}

	// 前端单页应用
	r.Handle("/*", s.spa())

	// ⚠️ chi 只认标准 HTTP 方法，PROPFIND 之类会被它直接判 405，
	// 所以 DAV 必须挂在 chi 外面的标准库 mux 上。
	dav := requestLogger(s.davAuth(davH))
	outer := http.NewServeMux()
	outer.Handle("/dav", dav)
	outer.Handle("/dav/", dav)
	outer.Handle("/", r)
	return outer
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		p := r.URL.Path
		if strings.HasPrefix(p, "/assets/") {
			return // 静态资源太吵，不记
		}
		slog.Info("http", "method", r.Method, "path", p, "status", ww.Status(), "ms", time.Since(start).Milliseconds())
	})
}

// spa 提供打包好的前端；未构建时给一句提示而不是白屏。
func (s *Server) spa() http.Handler {
	if s.ui == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>panhub</title>
<body style="font-family:system-ui;padding:40px">
<h2>panhub 后端已启动</h2>
<p>前端还没有构建。在 <code>web/</code> 目录执行：</p>
<pre>npm install
npm run build</pre>
<p>或者直接用 API / WebDAV：<code>/dav</code></p>
</body>`)
		})
	}
	files := http.FileServer(http.FS(s.ui))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if up != "" && up != "." {
			if _, err := fs.Stat(s.ui, up); err == nil {
				files.ServeHTTP(w, r)
				return
			}
		}
		// 前端路由回退到 index.html
		clone := r.Clone(r.Context())
		clone.URL.Path = "/"
		files.ServeHTTP(w, clone)
	})
}
