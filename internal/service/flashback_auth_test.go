package service

import (
	"strings"
	"testing"

	"db-flashback/internal/service/dto"
	"db-flashback/internal/storage/flashback"
)

func TestNewSessionToken(t *testing.T) {
	a, err := newSessionToken()
	if err != nil || len(a) != 64 {
		t.Fatalf("token=%s err=%v", a, err)
	}
	b, err := newSessionToken()
	if err != nil || a == b {
		t.Fatal("tokens should be unique")
	}
}

func TestFlashbackValidUsername(t *testing.T) {
	if err := flashbackValidUsername("ab"); err != nil {
		t.Fatal(err)
	}
	if err := flashbackValidUsername("a"); err == nil {
		t.Fatal("too short")
	}
	if err := flashbackValidUsername("bad name"); err == nil {
		t.Fatal("space not allowed")
	}
}

func TestFlashbackIsAdmin(t *testing.T) {
	if !flashbackIsAdmin("admin") || !flashbackIsAdmin("Admin") {
		t.Fatal("admin should be admin")
	}
	if flashbackIsAdmin("dba1") || flashbackIsAdmin("") {
		t.Fatal("ordinary user is not admin")
	}
}

func TestSetUserEnabledRequiresAdmin(t *testing.T) {
	s := NewFlashbackImpl()
	if _, err := s.SetUserEnabled(nil, "alice", false); err == nil || !strings.Contains(err.Error(), "仅管理员") {
		t.Fatalf("want admin only: %v", err)
	}
}

func TestUnlockUserRequiresAdmin(t *testing.T) {
	s := NewFlashbackImpl()
	if _, err := s.UnlockUser(nil, "alice"); err == nil || !strings.Contains(err.Error(), "仅管理员") {
		t.Fatalf("want admin only: %v", err)
	}
}

func TestLoginFailRemain(t *testing.T) {
	if loginFailRemain(1) != 2 || loginFailRemain(2) != 1 || loginFailRemain(3) != 0 {
		t.Fatalf("remain after 1/2/3 fails: %d %d %d", loginFailRemain(1), loginFailRemain(2), loginFailRemain(3))
	}
	if loginFailRemain(0) != flashbackMaxLoginFail {
		t.Fatalf("remain after 0: %d", loginFailRemain(0))
	}
}

func TestNoteLoginFailSkipsAdminAndUnknown(t *testing.T) {
	s := NewFlashbackImpl()
	if err := s.noteLoginFail(nil, nil); err == nil || err.Error() != "用户名或密码错误" {
		t.Fatalf("unknown: %v", err)
	}
	if err := s.noteLoginFail(nil, &flashback.UserRow{Username: "admin"}); err == nil || err.Error() != "用户名或密码错误" {
		t.Fatalf("admin: %v", err)
	}
}

func TestCreateUserRequiresAdmin(t *testing.T) {
	s := NewFlashbackImpl()
	if _, err := s.CreateUser(nil, &dto.FlashbackUserCreate{Username: "alice", Password: "abcdef"}); err == nil || !strings.Contains(err.Error(), "仅管理员") {
		t.Fatalf("want admin only: %v", err)
	}
	if err := s.DeleteUser(nil, "alice"); err == nil || !strings.Contains(err.Error(), "仅管理员") {
		t.Fatalf("delete want admin only: %v", err)
	}
}

func TestChangePasswordValidation(t *testing.T) {
	s := NewFlashbackImpl()
	if err := s.ChangePassword(nil, nil); err == nil || !strings.Contains(err.Error(), "请求为空") {
		t.Fatalf("nil req: %v", err)
	}
	if err := s.ChangePassword(nil, &dto.FlashbackPasswordReq{OldPassword: "x", NewPassword: "yyyyyy"}); err == nil || !strings.Contains(err.Error(), "未登录") {
		t.Fatalf("no user: %v", err)
	}
}
