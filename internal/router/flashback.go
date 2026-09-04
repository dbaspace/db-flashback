package router

import (
	"github.com/gin-gonic/gin"

	v1 "db-flashback/internal/router/api/v1"
	"db-flashback/internal/service"
)

func RegisterFlashbackRouter(app *gin.Engine) {
	h := v1.NewFlashbackHandler()
	registerFlashbackRoutes(app.Group("/api/v1/flashback"), h)
}

func registerFlashbackRoutes(rt *gin.RouterGroup, h *v1.FlashbackHandler) {
	rt.POST("/auth/login", h.Login)
	rt.Use(v1.RequireAuth())
	rt.POST("/auth/logout", h.Logout)
	rt.GET("/auth/me", h.Me)
	rt.PUT("/auth/password", h.ChangePassword)
	rt.GET("/auth/users", h.ListUsers)
	rt.POST("/auth/users", h.CreateUser)
	rt.PUT("/auth/users/:username/perms", h.SaveUserPerms)
	rt.PUT("/auth/users/:username/status", h.SetUserEnabled)
	rt.PUT("/auth/users/:username/unlock", h.UnlockUser)
	rt.DELETE("/auth/users/:username", h.DeleteUser)
	rt.GET("/dashboard", v1.RequirePage(service.PageDashboard, service.PermView), h.Dashboard)
	rt.GET("/instances", v1.RequirePageAny(service.PermView, service.PageDashboard, service.PageInstances, service.PageTasks, service.PageTools), h.ListInstances)
	rt.POST("/instances", v1.RequirePage(service.PageInstances, service.PermOperate), h.SaveInstance)
	rt.PUT("/instances", v1.RequirePage(service.PageInstances, service.PermOperate), h.SaveInstance)
	rt.PUT("/instances/:id", v1.RequirePage(service.PageInstances, service.PermOperate), h.SaveInstance)
	rt.DELETE("/instances/:id", v1.RequirePage(service.PageInstances, service.PermOperate), h.DeleteInstance)
	rt.GET("/cloud-settings", v1.RequirePage(service.PageOps, service.PermView), h.CloudSettings)
	rt.PUT("/cloud-settings", v1.RequirePage(service.PageOps, service.PermOperate), h.SaveCloudSettings)
	rt.POST("/tasks/precheck", v1.RequirePage(service.PageTasks, service.PermOperate), h.Precheck)
	rt.POST("/tasks/selftest", v1.RequirePage(service.PageTools, service.PermOperate), h.Selftest)
	rt.POST("/tasks", v1.RequirePage(service.PageTasks, service.PermOperate), h.Create)
	rt.GET("/tasks", v1.RequirePage(service.PageHistory, service.PermView), h.List)
	rt.GET("/tasks/:id", v1.RequirePage(service.PageHistory, service.PermView), h.Get)
	rt.GET("/tasks/:id/sql", v1.RequirePage(service.PageHistory, service.PermView), h.ListSQL)
	rt.GET("/tasks/:id/logs", v1.RequirePage(service.PageHistory, service.PermView), h.ListLogs)
	rt.GET("/tasks/:id/artifacts", v1.RequirePage(service.PageHistory, service.PermView), h.ListArtifacts)
	rt.GET("/tasks/:id/artifacts/file", v1.RequirePage(service.PageHistory, service.PermView), h.GetArtifact)
	rt.POST("/pdu/discover", v1.RequirePage(service.PageTasks, service.PermOperate), h.DiscoverPDU)
}
