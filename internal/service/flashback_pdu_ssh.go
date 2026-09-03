package service

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"db-flashback/internal/config"
)

type flashbackPDUSSH struct {
	Host string
	User string
	Port int
}

var (
	flashbackPDUSSHIdentRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	flashbackPDUSSHHostRe  = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)
)

func flashbackPDUSSHFromInstance(inst config.InstanceConfig) flashbackPDUSSH {
	user := strings.TrimSpace(inst.SSHUser)
	if user == "" {
		if cfg := runtimeConfig(); cfg != nil {
			user = strings.TrimSpace(cfg.Flashback.SSHUser)
		}
	}
	if user == "" {
		user = strings.TrimSpace(inst.User)
	}
	if user == "" {
		user = "postgres"
	}
	port := inst.SSHPort
	if port <= 0 && runtimeConfig() != nil && runtimeConfig().Flashback.SSHPort > 0 {
		port = runtimeConfig().Flashback.SSHPort
	}
	if port <= 0 {
		port = 22
	}
	return flashbackPDUSSH{Host: strings.TrimSpace(inst.Host), User: user, Port: port}
}

func flashbackPDUHostIsLocal(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" || host == "127.0.0.1" || host == "localhost" || host == "::1" {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP == nil {
			continue
		}
		if strings.EqualFold(ipnet.IP.String(), host) {
			return true
		}
	}
	return false
}

func (s flashbackPDUSSH) valid() error {
	if s.Host == "" {
		return fmt.Errorf("实例 host 为空")
	}
	if !flashbackPDUSSHHostRe.MatchString(s.Host) {
		return fmt.Errorf("实例 host 非法: %s", s.Host)
	}
	if !flashbackPDUSSHIdentRe.MatchString(s.User) {
		return fmt.Errorf("ssh 用户非法: %s", s.User)
	}
	if s.Port <= 0 || s.Port > 65535 {
		return fmt.Errorf("ssh 端口非法: %d", s.Port)
	}
	return nil
}

func (s flashbackPDUSSH) spec() string {
	return fmt.Sprintf("%s@%s:%d", s.User, s.Host, s.Port)
}

func flashbackPDURemotePathOK(raw string) error {
	path := filepath.Clean(strings.TrimSpace(raw))
	if path == "" || path == "." {
		return fmt.Errorf("远程路径为空")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("远程路径必须是绝对路径: %s", path)
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("远程路径非法: %s", path)
	}
	return nil
}

func flashbackPDUProbeSSH(ctx context.Context, s flashbackPDUSSH) error {
	if err := s.valid(); err != nil {
		return err
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		return fmt.Errorf("本机没有 ssh，无法远程拉日志")
	}
	pctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(pctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=8",
		"-p", strconv.Itoa(s.Port),
		s.User+"@"+s.Host,
		"true")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("SSH 互信未打通 %s: %v %s", s.spec(), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func flashbackPDURSyncRemote(ctx context.Context, s flashbackPDUSSH, remoteDir, localDir string, excludes []string) error {
	if err := s.valid(); err != nil {
		return err
	}
	if err := flashbackPDURemotePathOK(remoteDir); err != nil {
		return err
	}
	if _, err := exec.LookPath("rsync"); err != nil {
		return fmt.Errorf("本机没有 rsync，无法远程拉日志")
	}
	if err := os.MkdirAll(localDir, 0o700); err != nil {
		return err
	}
	sshE := fmt.Sprintf("ssh -o BatchMode=yes -o ConnectTimeout=15 -p %d", s.Port)
	remote := fmt.Sprintf("%s@%s:%s/", s.User, s.Host, strings.TrimRight(filepath.Clean(remoteDir), "/"))
	local := strings.TrimRight(filepath.Clean(localDir), "/") + "/"
	args := []string{"-a", "--protect-args"}
	for _, ex := range excludes {
		ex = strings.TrimSpace(ex)
		if ex == "" || strings.ContainsAny(ex, " \t\n") {
			continue
		}
		args = append(args, "--exclude="+ex)
	}
	args = append(args, "-e", sshE, remote, local)
	cmd := exec.CommandContext(ctx, "rsync", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rsync %s → %s: %v %s", remote, local, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func flashbackPDULookupSSH(instanceID string) (flashbackPDUSSH, bool) {
	inst, err := lookupConfiguredInstance(instanceID)
	if err != nil {
		return flashbackPDUSSH{}, false
	}
	s := flashbackPDUSSHFromInstance(inst)
	if s.Host == "" {
		return flashbackPDUSSH{}, false
	}
	return s, true
}
