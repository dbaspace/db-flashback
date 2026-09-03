package service

import (
	"archive/zip"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

type flashbackPGRuntime struct {
	Major   int
	Version string
	Image   string
	DSN     string
	Kind    string // docker | binary
	cleanup func()
}

type flashbackMatrixSpec struct {
	Major int
	Zonky string
	Image string
	Port  int
}

func (r *flashbackPGRuntime) Close() {
	if r != nil && r.cleanup != nil {
		r.cleanup()
		r.cleanup = nil
	}
}

var flashbackMatrixVersions = []flashbackMatrixSpec{
	{12, "12.22.0", "postgres:12-alpine", 15412},
	{13, "13.21.0", "postgres:13-alpine", 15413},
	{14, "14.18.0", "postgres:14-alpine", 15414},
	{15, "15.13.0", "postgres:15-alpine", 15415},
	{16, "16.9.0", "postgres:16-alpine", 15416},
	{17, "17.5.0", "postgres:17-alpine", 15417},
	{18, "18.3.0", "postgres:18-alpine", 15418},
	{19, "19beta3", "postgres:19beta3-alpine", 15419},
}

func flashbackDockerAvailable() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

func flashbackMatrixSelected() []flashbackMatrixSpec {
	raw := strings.TrimSpace(os.Getenv("FLASHBACK_PG_MATRIX_MAJORS"))
	if raw == "" {
		return flashbackMatrixVersions
	}
	var out []flashbackMatrixSpec
	for _, p := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			continue
		}
		if spec, ok := flashbackLookupMatrixSpec(n); ok {
			out = append(out, spec)
		}
	}
	return out
}

func flashbackStartPGMatrix(major int) (*flashbackPGRuntime, error) {
	spec, ok := flashbackLookupMatrixSpec(major)
	if !ok {
		return nil, fmt.Errorf("unsupported major %d", major)
	}
	if flashbackDockerAvailable() {
		rt, err := flashbackStartPGDocker(spec)
		if err == nil {
			return rt, nil
		}
		if rt != nil {
			rt.Close()
		}
		return nil, fmt.Errorf("docker 启动 PG%d 失败: %w", major, err)
	}
	return flashbackStartPGBinary(spec)
}

func flashbackLookupMatrixSpec(major int) (flashbackMatrixSpec, bool) {
	for _, s := range flashbackMatrixVersions {
		if s.Major == major {
			return s, true
		}
	}
	return flashbackMatrixSpec{}, false
}

func flashbackStartPGDocker(spec flashbackMatrixSpec) (*flashbackPGRuntime, error) {
	name := fmt.Sprintf("hub-fb-pg%d", spec.Major)
	_ = exec.Command("docker", "rm", "-f", name).Run()
	cmd := exec.Command("docker", "run", "-d", "--name", name, "--rm",
		"-e", "POSTGRES_PASSWORD=flashback",
		"-e", "POSTGRES_USER=postgres",
		"-e", "POSTGRES_DB=fbtest",
		"-p", fmt.Sprintf("127.0.0.1:%d:5432", spec.Port),
		spec.Image,
		"postgres", "-c", "wal_level=logical", "-c", "full_page_writes=on",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	rt := &flashbackPGRuntime{
		Major: spec.Major, Version: spec.Image, Image: spec.Image, Kind: "docker",
		DSN: fmt.Sprintf("host=127.0.0.1 port=%d user=postgres password=flashback dbname=fbtest sslmode=disable", spec.Port),
		cleanup: func() {
			_ = exec.Command("docker", "rm", "-f", name).Run()
		},
	}
	if err := flashbackWaitPG(rt.DSN, 90*time.Second); err != nil {
		rt.Close()
		return nil, err
	}
	return rt, nil
}

func flashbackZonkyPlatform() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "darwin-arm64v8"
	case "darwin/amd64":
		return "darwin-amd64"
	case "linux/amd64":
		return "linux-amd64"
	case "linux/arm64":
		return "linux-arm64v8"
	default:
		return ""
	}
}

func flashbackZonkyPlatformFor(major int) (plat string, rosetta bool) {
	plat = flashbackZonkyPlatform()
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" && major <= 13 {
		return "darwin-amd64", true
	}
	return plat, false
}

func flashbackMaybeRosetta(rosetta bool, name string, args ...string) *exec.Cmd {
	if rosetta {
		return exec.Command("arch", append([]string{"-x86_64", name}, args...)...)
	}
	return exec.Command(name, args...)
}

func flashbackStartPGBinary(spec flashbackMatrixSpec) (*flashbackPGRuntime, error) {
	plat, rosetta := flashbackZonkyPlatformFor(spec.Major)
	if plat == "" {
		return nil, fmt.Errorf("无 Docker，且不支持 %s/%s 的官方二进制回退", runtime.GOOS, runtime.GOARCH)
	}
	base := filepath.Join(os.TempDir(), "hub-fb-pg-matrix")
	verDir := filepath.Join(base, spec.Zonky)
	binDir := filepath.Join(verDir, "pg")
	dataDir := filepath.Join(verDir, "data")
	runDir := filepath.Join(verDir, "run")
	_ = os.RemoveAll(dataDir)
	_ = os.RemoveAll(runDir)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(binDir, "bin", "initdb")); err != nil {
		if err := flashbackFetchPGBinaries(spec, plat, binDir); err != nil {
			return nil, err
		}
	}
	pw := filepath.Join(runDir, "pwfile")
	if err := os.WriteFile(pw, []byte("flashback"), 0o600); err != nil {
		return nil, err
	}
	initdb := filepath.Join(binDir, "bin", "initdb")
	pgctl := filepath.Join(binDir, "bin", "pg_ctl")
	init := flashbackMaybeRosetta(rosetta, initdb, "-A", "password", "-U", "postgres", "-D", dataDir, "--pwfile="+pw, "--encoding=UTF8")
	if out, err := init.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("initdb PG%d: %w: %s", spec.Major, err, strings.TrimSpace(string(out)))
	}
	logFile := filepath.Join(runDir, "pg.log")
	start := flashbackMaybeRosetta(rosetta, pgctl, "-D", dataDir, "-l", logFile, "-o",
		fmt.Sprintf("-p %d -c wal_level=logical -c full_page_writes=on -c listen_addresses=127.0.0.1", spec.Port),
		"start")
	if out, err := start.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("pg_ctl start PG%d: %w: %s", spec.Major, err, strings.TrimSpace(string(out)))
	}
	rt := &flashbackPGRuntime{
		Major: spec.Major, Version: spec.Zonky, Kind: "binary",
		DSN: fmt.Sprintf("host=127.0.0.1 port=%d user=postgres password=flashback dbname=postgres sslmode=disable", spec.Port),
		cleanup: func() {
			_ = flashbackMaybeRosetta(rosetta, pgctl, "-D", dataDir, "-m", "fast", "stop").Run()
			_ = os.RemoveAll(dataDir)
			_ = os.RemoveAll(runDir)
		},
	}
	if err := flashbackWaitPG(rt.DSN, 60*time.Second); err != nil {
		rt.Close()
		return nil, err
	}
	return rt, nil
}

func flashbackFetchPGBinaries(spec flashbackMatrixSpec, plat, dest string) error {
	if spec.Major >= 19 {
		if err := flashbackFetchPostgresApp19(dest); err == nil {
			return nil
		}
		return flashbackBuildPG19FromSource(dest)
	}
	return flashbackFetchZonky(plat, spec.Zonky, dest)
}

const flashbackPG19SourceURL = "https://ftp.postgresql.org/pub/source/v19beta3/postgresql-19beta3.tar.gz"

func flashbackBuildPG19FromSource(dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	work := dest + "-src"
	_ = os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(work)
	tgz := filepath.Join(work, "postgresql-19beta3.tar.gz")
	if cached := "/tmp/postgresql-19beta3.tar.gz"; fileExists(cached) {
		if err := exec.Command("cp", cached, tgz).Run(); err != nil {
			return err
		}
	} else if err := flashbackHTTPDownload(flashbackPG19SourceURL, tgz); err != nil {
		return fmt.Errorf("下载 PG19 源码: %w", err)
	}
	if out, err := exec.Command("tar", "-xzf", tgz, "-C", work).CombinedOutput(); err != nil {
		return fmt.Errorf("解压源码: %w: %s", err, strings.TrimSpace(string(out)))
	}
	src := filepath.Join(work, "postgresql-19beta3")
	cfg := exec.Command("./configure", "--prefix="+dest, "--without-icu", "--without-readline")
	cfg.Dir = src
	if out, err := cfg.CombinedOutput(); err != nil {
		return fmt.Errorf("configure PG19: %w: %s", err, strings.TrimSpace(string(out)))
	}
	ncpu := runtime.NumCPU()
	if ncpu < 2 {
		ncpu = 2
	}
	mk := exec.Command("make", "-j"+strconv.Itoa(ncpu))
	mk.Dir = src
	if out, err := mk.CombinedOutput(); err != nil {
		return fmt.Errorf("make PG19: %w: %s", err, strings.TrimSpace(string(out)))
	}
	ins := exec.Command("make", "install")
	ins.Dir = src
	if out, err := ins.CombinedOutput(); err != nil {
		return fmt.Errorf("make install PG19: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if _, err := os.Stat(filepath.Join(dest, "bin", "initdb")); err != nil {
		return fmt.Errorf("编译后无 initdb: %w", err)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

const flashbackPG19AppDMG = "https://github.com/PostgresApp/PostgresApp/releases/download/v3alpha8/Postgres-3alpha8-19.dmg"

func flashbackFetchPostgresApp19(dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	dmg := dest + ".dmg"
	if err := flashbackHTTPDownload(flashbackPG19AppDMG, dmg); err != nil {
		return fmt.Errorf("下载 PostgreSQL 19 Beta: %w", err)
	}
	defer os.Remove(dmg)
	mnt := dest + ".mnt"
	_ = os.RemoveAll(mnt)
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return err
	}
	attach := exec.Command("hdiutil", "attach", dmg, "-nobrowse", "-readonly", "-mountpoint", mnt)
	if out, err := attach.CombinedOutput(); err != nil {
		return fmt.Errorf("挂载 19 dmg: %w: %s", err, strings.TrimSpace(string(out)))
	}
	defer func() {
		_ = exec.Command("hdiutil", "detach", mnt, "-quiet", "-force").Run()
		_ = os.RemoveAll(mnt)
	}()
	initdb, err := flashbackFindInitdb(mnt)
	if err != nil {
		return fmt.Errorf("19 dmg 内无 initdb: %w", err)
	}
	src := filepath.Dir(filepath.Dir(initdb))
	if out, err := exec.Command("ditto", src, dest).CombinedOutput(); err != nil {
		return fmt.Errorf("复制 PG19 二进制: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if _, err := os.Stat(filepath.Join(dest, "bin", "initdb")); err != nil {
		return fmt.Errorf("复制后仍无 initdb: %w", err)
	}
	return nil
}

func flashbackFetchZonky(plat, version, dest string) error {
	url := fmt.Sprintf("https://repo1.maven.org/maven2/io/zonky/test/postgres/embedded-postgres-binaries-%s/%s/embedded-postgres-binaries-%s-%s.jar",
		plat, version, plat, version)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	jar := dest + ".jar"
	if err := flashbackHTTPDownload(url, jar); err != nil {
		return fmt.Errorf("下载 PG %s: %w", version, err)
	}
	defer os.Remove(jar)
	zr, err := zip.OpenReader(jar)
	if err != nil {
		return err
	}
	defer zr.Close()
	var txz string
	for _, f := range zr.File {
		name := filepath.Base(f.Name)
		if strings.HasSuffix(name, ".txz") || strings.HasSuffix(name, ".tar.xz") {
			txz = filepath.Join(dest, name)
			if err := flashbackZipExtractFile(f, txz); err != nil {
				return err
			}
			break
		}
	}
	if txz == "" {
		return fmt.Errorf("jar 内没有 txz：%s", url)
	}
	cmd := exec.Command("tar", "-xJf", txz, "-C", dest)
	out, err := cmd.CombinedOutput()
	_ = os.Remove(txz)
	if err != nil {
		return fmt.Errorf("解压 %s: %w: %s", txz, err, strings.TrimSpace(string(out)))
	}
	if _, err := os.Stat(filepath.Join(dest, "bin", "initdb")); err != nil {
		found, err := flashbackFindInitdb(dest)
		if err != nil {
			return err
		}
		if filepath.Dir(filepath.Dir(found)) != dest {
			// 二进制在子目录，提升一层不好做；只要能找到即可，调用方用 dest/bin。
			return fmt.Errorf("initdb 不在 %s/bin", dest)
		}
	}
	return nil
}

func flashbackFindInitdb(root string) (string, error) {
	var found string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if info.Name() == "initdb" {
			found = path
			return io.EOF
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("未找到 initdb")
	}
	return found, nil
}

func flashbackZipExtractFile(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, rc)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func flashbackHTTPDownload(url, dest string) error {
	if _, err := exec.LookPath("curl"); err == nil {
		cmd := exec.Command("curl", "-fL", "--http1.1", "--retry", "3", "--retry-delay", "2",
			"--connect-timeout", "30", "--max-time", "1200",
			"-A", "Mozilla/5.0", "-H", "Cookie: oraclelicense=accept-securebackup-cookie",
			"-o", dest, url)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("curl: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	cli := &http.Client{Timeout: 20 * time.Minute}
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, url)
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func flashbackWaitPG(dsn string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			last = err
			time.Sleep(400 * time.Millisecond)
			continue
		}
		db.SetConnMaxLifetime(3 * time.Second)
		err = db.Ping()
		_ = db.Close()
		if err == nil {
			return nil
		}
		last = err
		time.Sleep(400 * time.Millisecond)
	}
	if last == nil {
		last = fmt.Errorf("timeout")
	}
	return fmt.Errorf("等待 PostgreSQL: %w", last)
}

func flashbackUnusedPort(preferred int) int {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(preferred))
	if err == nil {
		_ = ln.Close()
		return preferred
	}
	ln, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return preferred
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}
