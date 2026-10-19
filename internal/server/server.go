package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"time"

	"panhub/internal/config"
	"panhub/internal/storage"
	"panhub/internal/store"
	"panhub/internal/task"
	"panhub/internal/vfs"
)

// Server 汇总 HTTP 层依赖。
type Server struct {
	cfg     *config.Config
	v       *vfs.VFS
	st      *store.Store
	runner  *task.Runner
	tokens  *tokenStore
	version string
	started time.Time
	ui      fs.FS
}

// New 构造 Server。
func New(cfg *config.Config, v *vfs.VFS, st *store.Store, runner *task.Runner, version string, ui fs.FS) *Server {
	return &Server{
		cfg:     cfg,
		v:       v,
		st:      st,
		runner:  runner,
		tokens:  newTokenStore(cfg.Server.AdminPassword),
		version: version,
		started: time.Now(),
		ui:      ui,
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// mapError 把驱动哨兵错误映射成合适的 HTTP 状态码。
func mapError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		writeError(w, http.StatusNotFound, "路径不存在")
	case errors.Is(err, vfs.ErrReadOnly):
		writeError(w, http.StatusForbidden, "该挂载点是只读的")
	case errors.Is(err, storage.ErrNotSupported):
		writeError(w, http.StatusBadRequest, "该驱动不支持此操作")
	case errors.Is(err, storage.ErrLoginRequired):
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, storage.ErrNotConfigured), errors.Is(err, storage.ErrNotImplemented):
		writeError(w, http.StatusNotImplemented, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func readJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}
