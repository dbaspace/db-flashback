package router

import (
	"github.com/gin-gonic/gin"

	v1 "db-flashback/internal/router/api/v1"
)

func RegisterFlashbackRouter(app *gin.Engine) {
	h := v1.NewFlashbackHandler()
	registerFlashbackRoutes(app.Group("/api/v1/flashback"), h)
}

func registerFlashbackRoutes(rt *gin.RouterGroup, h *v1.FlashbackHandler) {
	rt.GET("/instances", h.ListInstances)
	rt.POST("/instances", h.SaveInstance)
	rt.PUT("/instances/:id", h.SaveInstance)
	rt.DELETE("/instances/:id", h.DeleteInstance)
	rt.GET("/cloud-settings", h.CloudSettings)
	rt.PUT("/cloud-settings", h.SaveCloudSettings)
	rt.POST("/tasks/precheck", h.Precheck)
	rt.POST("/tasks/selftest", h.Selftest)
	rt.POST("/tasks", h.Create)
	rt.GET("/tasks", h.List)
	rt.GET("/tasks/:id", h.Get)
	rt.GET("/tasks/:id/sql", h.ListSQL)
	rt.GET("/tasks/:id/logs", h.ListLogs)
	rt.GET("/tasks/:id/artifacts", h.ListArtifacts)
	rt.GET("/tasks/:id/artifacts/file", h.GetArtifact)
	rt.POST("/pdu/discover", h.DiscoverPDU)
}
