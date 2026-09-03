package v1

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"db-flashback/internal/service"
	"db-flashback/internal/service/dto"
	"db-flashback/pkg/ginplus/response"
)

type FlashbackHandler struct{}

func NewFlashbackHandler() *FlashbackHandler { return &FlashbackHandler{} }

func flashbackWriteOK(c *gin.Context, result any) {
	response.Resp(c.Writer, &dto.FlashbackEnvelope{
		BaseResponse: response.BaseResponse{Code: response.CodeOK},
		Result:       result,
	})
}

func (h *FlashbackHandler) ListInstances(c *gin.Context) {
	flashbackWriteOK(c, service.NewFlashbackImpl().ListInstances(c.Request.Context()))
}

func (h *FlashbackHandler) SaveInstance(c *gin.Context) {
	req := &dto.FlashbackInstanceSave{}
	if err := c.ShouldBindJSON(req); err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	if id := strings.TrimSpace(c.Param("id")); id != "" {
		req.ID = id
	}
	data, err := service.NewFlashbackImpl().SaveInstance(c.Request.Context(), req)
	if err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	flashbackWriteOK(c, data)
}

func (h *FlashbackHandler) DeleteInstance(c *gin.Context) {
	if err := service.NewFlashbackImpl().DeleteInstance(c.Request.Context(), c.Param("id")); err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	flashbackWriteOK(c, gin.H{"ok": true})
}

func (h *FlashbackHandler) CloudSettings(c *gin.Context) {
	flashbackWriteOK(c, service.NewFlashbackImpl().CloudSettings(c.Request.Context()))
}

func (h *FlashbackHandler) SaveCloudSettings(c *gin.Context) {
	req := &dto.FlashbackCloudSettingsSave{}
	if err := c.ShouldBindJSON(req); err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	data, err := service.NewFlashbackImpl().SaveCloudSettings(c.Request.Context(), req)
	if err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	flashbackWriteOK(c, data)
}

func (h *FlashbackHandler) Precheck(c *gin.Context) {
	req := &dto.FlashbackTaskReq{}
	if err := c.ShouldBindJSON(req); err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	data, err := service.NewFlashbackImpl().Precheck(c, req)
	if err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	flashbackWriteOK(c, data)
}

func (h *FlashbackHandler) Selftest(c *gin.Context) {
	req := &dto.FlashbackSelftestReq{}
	if err := c.ShouldBindJSON(req); err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	data, err := service.NewFlashbackImpl().Selftest(c, req)
	if err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	flashbackWriteOK(c, data)
}

func (h *FlashbackHandler) Create(c *gin.Context) {
	req := &dto.FlashbackTaskReq{}
	if err := c.ShouldBindJSON(req); err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	data, err := service.NewFlashbackImpl().Create(c, req)
	if err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	flashbackWriteOK(c, data)
}

func (h *FlashbackHandler) List(c *gin.Context) {
	data, err := service.NewFlashbackImpl().List(c)
	if err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	flashbackWriteOK(c, data)
}

func (h *FlashbackHandler) Get(c *gin.Context) {
	data, err := service.NewFlashbackImpl().Get(c, c.Param("id"))
	if err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	flashbackWriteOK(c, data)
}

func (h *FlashbackHandler) ListSQL(c *gin.Context) {
	data, err := service.NewFlashbackImpl().ListSQL(c, c.Param("id"))
	if err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	flashbackWriteOK(c, data)
}

func (h *FlashbackHandler) ListLogs(c *gin.Context) {
	data, err := service.NewFlashbackImpl().ListLogs(c, c.Param("id"))
	if err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	flashbackWriteOK(c, data)
}

func (h *FlashbackHandler) DiscoverPDU(c *gin.Context) {
	req := &dto.FlashbackPDUDiscoverReq{}
	if err := c.ShouldBindJSON(req); err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	data, err := service.NewFlashbackImpl().DiscoverPDU(c, req)
	if err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	flashbackWriteOK(c, data)
}

func (h *FlashbackHandler) ListArtifacts(c *gin.Context) {
	data, err := service.NewFlashbackImpl().ListArtifacts(c, c.Param("id"))
	if err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	flashbackWriteOK(c, data)
}

func (h *FlashbackHandler) GetArtifact(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	if name == "" {
		name = strings.TrimPrefix(c.Param("filepath"), "/")
	}
	if q := strings.TrimSpace(c.Query("name")); q != "" {
		name = q
	}
	path, err := service.NewFlashbackImpl().ArtifactFile(c, c.Param("id"), name)
	if err != nil {
		response.Resp400Error(c.Writer, err)
		return
	}
	c.FileAttachment(path, filepath.Base(path))
	if c.Writer.Status() == 0 {
		c.Status(http.StatusOK)
	}
}
