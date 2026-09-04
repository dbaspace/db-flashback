package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"db-flashback/internal/service/dto"
)

const (
	PageDashboard = "dashboard"
	PageTasks     = "tasks"
	PageHistory   = "history"
	PageInstances = "instances"
	PageOps       = "ops"
	PageTools     = "tools"
	PermView      = "view"
	PermOperate   = "operate"
)

func FlashbackPageCatalog() []dto.FlashbackPageInfo {
	return []dto.FlashbackPageInfo{
		{Key: PageDashboard, Name: "仪表盘"},
		{Key: PageTasks, Name: "闪回任务"},
		{Key: PageHistory, Name: "历史记录"},
		{Key: PageInstances, Name: "实例地址"},
		{Key: PageOps, Name: "运维中心"},
		{Key: PageTools, Name: "工具与集成"},
	}
}

func flashbackKnownPage(key string) bool {
	for _, p := range FlashbackPageCatalog() {
		if p.Key == key {
			return true
		}
	}
	return false
}

func parseStoredPerms(raw string) map[string]string {
	out := map[string]string{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return out
	}
	return normalizePerms(m)
}

func normalizePerms(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		k = strings.TrimSpace(k)
		v = strings.ToLower(strings.TrimSpace(v))
		if !flashbackKnownPage(k) {
			continue
		}
		if v != PermView && v != PermOperate {
			continue
		}
		out[k] = v
	}
	return out
}

func encodePerms(m map[string]string) (string, error) {
	n := normalizePerms(m)
	b, err := json.Marshal(n)
	if err != nil {
		return "{}", err
	}
	return string(b), nil
}

func adminPerms() map[string]string {
	out := map[string]string{}
	for _, p := range FlashbackPageCatalog() {
		out[p.Key] = PermOperate
	}
	return out
}

func effectivePerms(username, storedJSON string) map[string]string {
	if flashbackIsAdmin(username) {
		return adminPerms()
	}
	return parseStoredPerms(storedJSON)
}

func hasPageLevel(level, need string) bool {
	need = strings.ToLower(strings.TrimSpace(need))
	level = strings.ToLower(strings.TrimSpace(level))
	if need == PermView {
		return level == PermView || level == PermOperate
	}
	return need == PermOperate && level == PermOperate
}

func HasPagePerm(username, storedJSON, page, need string) bool {
	return hasPageLevel(effectivePerms(username, storedJSON)[page], need)
}

func loadUserPermsJSON(ctx context.Context, username string) (string, error) {
	if flashbackIsAdmin(username) {
		b, _ := json.Marshal(adminPerms())
		return string(b), nil
	}
	u, err := flashbackStore.GetUser(ctx, username)
	if err != nil {
		return "{}", err
	}
	if u == nil {
		return "{}", nil
	}
	return u.Perms, nil
}

func RequirePagePerm(c *gin.Context, page, need string) error {
	name := CurrentUsername(c)
	if name == "" {
		return fmt.Errorf("未登录")
	}
	if flashbackIsAdmin(name) {
		return nil
	}
	raw, err := loadUserPermsJSON(c.Request.Context(), name)
	if err != nil {
		return err
	}
	if !HasPagePerm(name, raw, page, need) {
		if need == PermOperate {
			return fmt.Errorf("没有该页面的操作权限")
		}
		return fmt.Errorf("没有该页面的查看权限")
	}
	return nil
}

func RequirePagePermAny(c *gin.Context, need string, pages ...string) error {
	name := CurrentUsername(c)
	if name == "" {
		return fmt.Errorf("未登录")
	}
	if flashbackIsAdmin(name) {
		return nil
	}
	raw, err := loadUserPermsJSON(c.Request.Context(), name)
	if err != nil {
		return err
	}
	for _, page := range pages {
		if HasPagePerm(name, raw, page, need) {
			return nil
		}
	}
	return fmt.Errorf("没有该页面的权限")
}

func (s *FlashbackImpl) SaveUserPerms(c *gin.Context, username string, req *dto.FlashbackUserPermsSave) (*dto.FlashbackUserView, error) {
	if err := requireAdmin(c); err != nil {
		return nil, err
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("用户名必填")
	}
	if flashbackIsAdmin(username) {
		return nil, fmt.Errorf("不能修改管理员权限")
	}
	var in map[string]string
	if req != nil {
		in = req.Perms
	}
	raw, err := encodePerms(in)
	if err != nil {
		return nil, err
	}
	if err := s.store.UpdateUserPerms(c.Request.Context(), username, raw); err != nil {
		return nil, err
	}
	u, err := s.store.GetUser(c.Request.Context(), username)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, fmt.Errorf("用户不存在")
	}
	v := flashbackUserViewOf(u.Username, u.Perms, u.Enabled, u.Locked, u.LoginFailCount, u.CreatedAt, u.UpdatedAt)
	return &v, nil
}
