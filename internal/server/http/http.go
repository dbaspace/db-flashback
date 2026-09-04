package http

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"db-flashback/internal/config"
	"db-flashback/internal/router"
	"db-flashback/internal/storage/databases"
	"db-flashback/web"
)

type HTTP struct {
	srv    *http.Server
	engine *gin.Engine
}

func NewHTTP(cfg *config.HTTPConfig) (*HTTP, error) {
	if cfg == nil {
		return nil, fmt.Errorf("invalid http config")
	}
	gin.SetMode(cfg.RunMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	engine.GET("/readyz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ready"}) })
	router.RegisterFlashbackRouter(engine)
	registerWebUI(engine)

	srv := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:      engine,
		ReadTimeout:  time.Duration(cfg.ReadTimeoutSec) * time.Second,
		WriteTimeout: time.Duration(cfg.WriteTimeoutSec) * time.Second,
	}
	srv.IdleTimeout = 2 * srv.ReadTimeout
	return &HTTP{srv: srv, engine: engine}, nil
}

func registerWebUI(engine *gin.Engine) {
	assets, err := fs.Sub(web.FS, ".")
	if err != nil {
		slog.Error("web ui embed failed", slog.Any("error", err))
		return
	}
	serve := func(name, contentType string) gin.HandlerFunc {
		return func(c *gin.Context) {
			body, err := fs.ReadFile(assets, name)
			if err != nil {
				c.Status(http.StatusNotFound)
				return
			}
			c.Header("Cache-Control", "no-cache")
			c.Data(http.StatusOK, contentType, body)
		}
	}
	engine.GET("/", serve("index.html", "text/html; charset=utf-8"))
	engine.GET("/app.css", serve("app.css", "text/css; charset=utf-8"))
	engine.GET("/app.js", serve("app.js", "application/javascript; charset=utf-8"))
	engine.GET("/login-bg.jpg", serve("login-bg.jpg", "image/jpeg"))
}

func (s *HTTP) Run() error {
	slog.Info("starting flashback server", slog.String("addr", s.srv.Addr))
	return s.srv.ListenAndServe()
}

func (s *HTTP) Shutdown(ctx context.Context) error {
	_ = databases.Close()
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}
