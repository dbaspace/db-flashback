package server

import (
	"context"
	"fmt"

	"github.com/gin-gonic/gin"

	"db-flashback/internal/config"
	"db-flashback/internal/server/http"
	"db-flashback/internal/service"
	"db-flashback/internal/storage/databases"
)

type SvrInstance struct {
	http   *http.HTTP
	ctx    context.Context
	cancel context.CancelFunc
	cfg    *config.SvrConfig
}

func NewSvr(cfg *config.SvrConfig) (*SvrInstance, error) {
	if cfg == nil {
		return nil, fmt.Errorf("invalid initial server instance")
	}
	if err := databases.MustOpen(cfg.DB); err != nil {
		return nil, err
	}
	service.InitRuntime(cfg)
	if err := service.FlashbackBootstrap(context.Background()); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	h, err := http.NewHTTP(&cfg.HTTP)
	if err != nil {
		cancel()
		return nil, err
	}
	return &SvrInstance{http: h, ctx: ctx, cancel: cancel, cfg: cfg}, nil
}

func (s *SvrInstance) Run() error {
	return s.http.Run()
}

func (s *SvrInstance) Shutdown(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}
	return s.http.Shutdown(ctx)
}

func GinMode(cfg *config.SvrConfig) string {
	if cfg == nil || cfg.HTTP.RunMode == "" {
		return gin.ReleaseMode
	}
	return cfg.HTTP.RunMode
}
