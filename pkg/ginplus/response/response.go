package response

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"db-flashback/pkg/errors"
)

const (
	CodeOK              = "0"
	MsgOK               = "ok"
	HTTP_REQUEST_ID_KEY = "X-Request-Id"
)

type BaseResponse struct {
	Code      string `json:"code"`
	RequestID string `json:"request_id,omitempty"`
}

func Resp(w http.ResponseWriter, data any) {
	jsonResp(w, data, http.StatusOK)
}

func Resp400Error(w http.ResponseWriter, err error) {
	slog.Debug("response 400 error", slog.Any("error", err))
	reqID := w.Header().Get(HTTP_REQUEST_ID_KEY)
	if e, ok := err.(*errors.Error); ok {
		if reqID != "" {
			e.Details["request_id"] = reqID
		}
		jsonResp(w, e, http.StatusBadRequest)
		return
	}
	eq := errors.ErrBadRequest()
	eq.SetError(err)
	if reqID != "" {
		eq.Details["request_id"] = reqID
	}
	jsonResp(w, eq, http.StatusBadRequest)
}

func jsonResp(w http.ResponseWriter, data any, code int) {
	body, err := json.Marshal(data)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	var out bytes.Buffer
	if json.Indent(&out, body, "", "  ") == nil {
		_, _ = w.Write(out.Bytes())
		return
	}
	_, _ = w.Write(body)
}

type PageReq struct {
	Current  int `json:"current" form:"current"`
	PageSize int `json:"page_size" form:"page_size"`
	Offset   int `json:"-"`
}

func GetPaginationParam(c *gin.Context) PageReq {
	l := PageReq{Current: 1, PageSize: 10}
	if v := c.Query("current"); v != "" {
		if iv, _ := strconv.Atoi(v); iv > 0 {
			l.Current = iv
		}
	}
	if v := c.Query("page_size"); v != "" {
		if iv, _ := strconv.Atoi(v); iv > 0 {
			l.PageSize = iv
		}
	}
	if l.PageSize > 2000 {
		l.PageSize = 2000
	}
	l.Offset = (l.Current - 1) * l.PageSize
	return l
}
