package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	secrypto "db-flashback/internal/crypto"
	"db-flashback/internal/service/dto"
	"db-flashback/internal/storage/flashback"
	"db-flashback/pkg/utils/log"

	"go.uber.org/zap"
)

const (
	flashbackSessionCookie = "flashback_sid"
	flashbackSessionTTL    = 24 * time.Hour
	flashbackDefaultUser   = "admin"
	flashbackDefaultPass   = "flashback"
	FlashbackCtxUser       = "flashback_user"
	flashbackMinPassword   = 6
	flashbackMaxLoginFail  = 3
)

func flashbackEnsureDefaultAdmin(ctx context.Context) {
	n, err := flashbackStore.CountUsers(ctx)
	if err != nil {
		log.Warn("flashback count users", zap.Error(err))
		return
	}
	if n > 0 {
		return
	}
	hash, err := secrypto.HashPassword(flashbackDefaultPass)
	if err != nil {
		log.Warn("flashback hash default admin", zap.Error(err))
		return
	}
	if err := flashbackStore.InsertUser(ctx, flashbackDefaultUser, hash); err != nil {
		log.Warn("flashback insert default admin", zap.Error(err))
		return
	}
	log.Info("flashback created default admin user")
}

func newSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func setSessionCookie(c *gin.Context, token string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     flashbackSessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func sessionToken(c *gin.Context) string {
	if c == nil {
		return ""
	}
	v, err := c.Cookie(flashbackSessionCookie)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

func CurrentUsername(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if v, ok := c.Get(FlashbackCtxUser); ok {
		if name, ok := v.(string); ok {
			return strings.TrimSpace(name)
		}
	}
	return ""
}

func (s *FlashbackImpl) Login(c *gin.Context, req *dto.FlashbackLoginReq) (*dto.FlashbackLoginView, error) {
	if req == nil {
		return nil, fmt.Errorf("请求为空")
	}
	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		return nil, fmt.Errorf("用户名或密码错误")
	}
	_ = s.store.DeleteExpiredSessions(c.Request.Context())
	user, err := s.store.GetUser(c.Request.Context(), username)
	if err != nil {
		return nil, err
	}
	if user != nil && !user.Enabled {
		return nil, fmt.Errorf("账号已禁用，请联系管理员")
	}
	if user != nil && user.Locked {
		return nil, fmt.Errorf("账号已锁定，请联系管理员解锁")
	}
	if user == nil || !secrypto.CheckPassword(user.Password, req.Password) {
		return nil, s.noteLoginFail(c.Request.Context(), user)
	}
	_ = s.store.ResetLoginFail(c.Request.Context(), user.Username)
	token, err := newSessionToken()
	if err != nil {
		return nil, err
	}
	if err := s.store.CreateSession(c.Request.Context(), token, user.Username, time.Now().Add(flashbackSessionTTL)); err != nil {
		return nil, err
	}
	setSessionCookie(c, token, int(flashbackSessionTTL.Seconds()))
	return flashbackLoginView(user.Username, user.Perms), nil
}

func (s *FlashbackImpl) Logout(c *gin.Context) error {
	if tok := sessionToken(c); tok != "" {
		_ = s.store.DeleteSession(c.Request.Context(), tok)
	}
	setSessionCookie(c, "", -1)
	return nil
}

func (s *FlashbackImpl) Me(c *gin.Context) (*dto.FlashbackLoginView, error) {
	name := CurrentUsername(c)
	if name == "" {
		return nil, fmt.Errorf("未登录")
	}
	raw, _ := loadUserPermsJSON(c.Request.Context(), name)
	return flashbackLoginView(name, raw), nil
}

func flashbackIsAdmin(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), flashbackDefaultUser)
}

func flashbackRole(name string) (role string, admin bool) {
	if flashbackIsAdmin(name) {
		return "admin", true
	}
	return "user", false
}

func flashbackLoginView(name, storedJSON string) *dto.FlashbackLoginView {
	role, admin := flashbackRole(name)
	return &dto.FlashbackLoginView{
		Username: name, Role: role, IsAdmin: admin,
		Perms: effectivePerms(name, storedJSON),
		Pages: FlashbackPageCatalog(),
	}
}

func flashbackUserViewOf(username, storedJSON string, enabled, locked bool, failCount int, created, updated time.Time) dto.FlashbackUserView {
	role, admin := flashbackRole(username)
	return dto.FlashbackUserView{
		Username: username, Role: role, IsAdmin: admin, Enabled: enabled, Locked: locked,
		LoginFailCount: failCount, Perms: effectivePerms(username, storedJSON),
		CreatedAt: created, UpdatedAt: updated,
	}
}

func loginFailRemain(failCount int) int {
	n := flashbackMaxLoginFail - failCount
	if n < 0 {
		return 0
	}
	return n
}

func (s *FlashbackImpl) noteLoginFail(ctx context.Context, user *flashback.UserRow) error {
	if user == nil || flashbackIsAdmin(user.Username) {
		return fmt.Errorf("用户名或密码错误")
	}
	n, locked, err := s.store.RecordLoginFail(ctx, user.Username, flashbackMaxLoginFail)
	if err != nil {
		return fmt.Errorf("用户名或密码错误")
	}
	if locked {
		_ = s.store.DeleteUserSessions(ctx, user.Username)
		return fmt.Errorf("账号已锁定，请联系管理员解锁")
	}
	return fmt.Errorf("用户名或密码错误，还可尝试 %d 次", loginFailRemain(n))
}

func requireAdmin(c *gin.Context) error {
	if !flashbackIsAdmin(CurrentUsername(c)) {
		return fmt.Errorf("仅管理员可管理用户")
	}
	return nil
}

func (s *FlashbackImpl) ChangePassword(c *gin.Context, req *dto.FlashbackPasswordReq) error {
	if req == nil {
		return fmt.Errorf("请求为空")
	}
	username := CurrentUsername(c)
	if username == "" {
		return fmt.Errorf("未登录")
	}
	if strings.TrimSpace(req.OldPassword) == "" {
		return fmt.Errorf("请输入原密码")
	}
	newPass := strings.TrimSpace(req.NewPassword)
	if len(newPass) < flashbackMinPassword {
		return fmt.Errorf("新密码至少 %d 位", flashbackMinPassword)
	}
	if req.OldPassword == newPass {
		return fmt.Errorf("新密码不能与原密码相同")
	}
	user, err := s.store.GetUser(c.Request.Context(), username)
	if err != nil {
		return err
	}
	if user == nil || !secrypto.CheckPassword(user.Password, req.OldPassword) {
		return fmt.Errorf("原密码不正确")
	}
	hash, err := secrypto.HashPassword(newPass)
	if err != nil {
		return err
	}
	if err := s.store.UpdateUserPassword(c.Request.Context(), user.Username, hash); err != nil {
		return err
	}
	_ = s.store.DeleteUserSessions(c.Request.Context(), user.Username)
	token, err := newSessionToken()
	if err != nil {
		return err
	}
	if err := s.store.CreateSession(c.Request.Context(), token, user.Username, time.Now().Add(flashbackSessionTTL)); err != nil {
		return err
	}
	setSessionCookie(c, token, int(flashbackSessionTTL.Seconds()))
	return nil
}

func flashbackValidUsername(name string) error {
	name = strings.TrimSpace(name)
	if n := len(name); n < 2 || n > 64 {
		return fmt.Errorf("用户名长度须为 2–64")
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.'
		if !ok {
			return fmt.Errorf("用户名只能含字母、数字、点、下划线、连字符")
		}
	}
	return nil
}

func (s *FlashbackImpl) ListUsers(c *gin.Context) ([]dto.FlashbackUserView, error) {
	if err := requireAdmin(c); err != nil {
		return nil, err
	}
	rows, err := s.store.ListUsers(c.Request.Context())
	if err != nil {
		return nil, err
	}
	out := make([]dto.FlashbackUserView, 0, len(rows))
	for _, r := range rows {
		out = append(out, flashbackUserViewOf(r.Username, r.Perms, r.Enabled, r.Locked, r.LoginFailCount, r.CreatedAt, r.UpdatedAt))
	}
	return out, nil
}

func (s *FlashbackImpl) CreateUser(c *gin.Context, req *dto.FlashbackUserCreate) (*dto.FlashbackUserView, error) {
	if err := requireAdmin(c); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, fmt.Errorf("请求为空")
	}
	username := strings.TrimSpace(req.Username)
	if flashbackIsAdmin(username) {
		return nil, fmt.Errorf("admin 为系统管理员，不能再创建同名账号")
	}
	if err := flashbackValidUsername(username); err != nil {
		return nil, err
	}
	pass := strings.TrimSpace(req.Password)
	if len(pass) < flashbackMinPassword {
		return nil, fmt.Errorf("密码至少 %d 位", flashbackMinPassword)
	}
	exist, err := s.store.GetUser(c.Request.Context(), username)
	if err != nil {
		return nil, err
	}
	if exist != nil {
		return nil, fmt.Errorf("用户已存在")
	}
	hash, err := secrypto.HashPassword(pass)
	if err != nil {
		return nil, err
	}
	if err := s.store.InsertUser(c.Request.Context(), username, hash); err != nil {
		return nil, err
	}
	saved, err := s.store.GetUser(c.Request.Context(), username)
	if err != nil {
		return nil, err
	}
	if saved == nil {
		return nil, fmt.Errorf("创建后未找到用户")
	}
	v := flashbackUserViewOf(saved.Username, saved.Perms, saved.Enabled, saved.Locked, saved.LoginFailCount, saved.CreatedAt, saved.UpdatedAt)
	return &v, nil
}

func (s *FlashbackImpl) SetUserEnabled(c *gin.Context, username string, enabled bool) (*dto.FlashbackUserView, error) {
	if err := requireAdmin(c); err != nil {
		return nil, err
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("用户名必填")
	}
	if flashbackIsAdmin(username) {
		return nil, fmt.Errorf("不能禁用管理员账号")
	}
	if err := s.store.SetUserEnabled(c.Request.Context(), username, enabled); err != nil {
		return nil, err
	}
	if enabled {
		_ = s.store.UnlockUser(c.Request.Context(), username)
	} else {
		_ = s.store.DeleteUserSessions(c.Request.Context(), username)
	}
	return s.userView(c.Request.Context(), username)
}

func (s *FlashbackImpl) UnlockUser(c *gin.Context, username string) (*dto.FlashbackUserView, error) {
	if err := requireAdmin(c); err != nil {
		return nil, err
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("用户名必填")
	}
	if flashbackIsAdmin(username) {
		return nil, fmt.Errorf("管理员账号无需解锁")
	}
	if err := s.store.UnlockUser(c.Request.Context(), username); err != nil {
		return nil, err
	}
	return s.userView(c.Request.Context(), username)
}

func (s *FlashbackImpl) userView(ctx context.Context, username string) (*dto.FlashbackUserView, error) {
	u, err := s.store.GetUser(ctx, username)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, fmt.Errorf("用户不存在")
	}
	v := flashbackUserViewOf(u.Username, u.Perms, u.Enabled, u.Locked, u.LoginFailCount, u.CreatedAt, u.UpdatedAt)
	return &v, nil
}

func (s *FlashbackImpl) DeleteUser(c *gin.Context, username string) error {
	if err := requireAdmin(c); err != nil {
		return err
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return fmt.Errorf("用户名必填")
	}
	if flashbackIsAdmin(username) {
		return fmt.Errorf("不能删除管理员账号")
	}
	if strings.EqualFold(username, CurrentUsername(c)) {
		return fmt.Errorf("不能删除当前登录账号")
	}
	n, err := s.store.CountUsers(c.Request.Context())
	if err != nil {
		return err
	}
	if n <= 1 {
		return fmt.Errorf("不能删除最后一个账号")
	}
	if err := s.store.DeleteUser(c.Request.Context(), username); err != nil {
		return err
	}
	_ = s.store.DeleteUserSessions(c.Request.Context(), username)
	return nil
}

func ResolveSessionUser(c *gin.Context) (string, error) {
	tok := sessionToken(c)
	if tok == "" {
		return "", fmt.Errorf("未登录")
	}
	name, err := flashbackStore.GetSessionUser(c.Request.Context(), tok)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("未登录")
	}
	u, err := flashbackStore.GetUser(c.Request.Context(), name)
	if err != nil {
		return "", err
	}
	if u == nil || !u.Enabled {
		_ = flashbackStore.DeleteSession(c.Request.Context(), tok)
		return "", fmt.Errorf("账号已禁用")
	}
	if u.Locked {
		_ = flashbackStore.DeleteSession(c.Request.Context(), tok)
		return "", fmt.Errorf("账号已锁定")
	}
	return name, nil
}
