package service

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

type flashbackMySQLMatrixSpec struct {
	Name  string
	Image string
	Port  int
}

// 5.7 → 当前 GA：8.0 / 8.4 LTS / 9.7 LTS / 26.7。与 PG 矩阵一样用 Docker 拉起。
var flashbackMySQLMatrixVersions = []flashbackMySQLMatrixSpec{
	{Name: "5.7", Image: "mysql:5.7", Port: 13357},
	{Name: "8.0", Image: "mysql:8.0", Port: 13380},
	{Name: "8.4", Image: "mysql:8.4", Port: 13384},
	{Name: "9.7", Image: "mysql:9.7", Port: 13397},
	{Name: "26.7", Image: "mysql:26.7", Port: 13326},
}

func flashbackMySQLMatrixSelected() []flashbackMySQLMatrixSpec {
	raw := strings.TrimSpace(os.Getenv("FLASHBACK_MYSQL_MATRIX_VERS"))
	if raw == "" {
		return flashbackMySQLMatrixVersions
	}
	want := map[string]struct{}{}
	for _, p := range strings.Split(raw, ",") {
		want[strings.TrimSpace(p)] = struct{}{}
	}
	var out []flashbackMySQLMatrixSpec
	for _, spec := range flashbackMySQLMatrixVersions {
		if _, ok := want[spec.Name]; ok {
			out = append(out, spec)
		}
	}
	return out
}

func flashbackMySQLMatrixDSN(port int, dbName string) string {
	cfg := mysqldriver.NewConfig()
	cfg.User = "root"
	cfg.Passwd = "flashback"
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("127.0.0.1:%d", port)
	cfg.DBName = dbName
	cfg.ParseTime = true
	cfg.Params = map[string]string{"charset": "utf8mb4"}
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 30 * time.Second
	cfg.WriteTimeout = 30 * time.Second
	return cfg.FormatDSN()
}

func flashbackStartMySQLMatrix(spec flashbackMySQLMatrixSpec) (creds flashbackMySQLCreds, cleanup func(), err error) {
	if !flashbackDockerAvailable() {
		return creds, nil, fmt.Errorf("需要 Docker 才能拉起 MySQL %s（与 PG 矩阵相同）", spec.Name)
	}
	return flashbackStartMySQLDocker(spec)
}

func flashbackStartMySQLDocker(spec flashbackMySQLMatrixSpec) (creds flashbackMySQLCreds, cleanup func(), err error) {
	name := "hub-fb-mysql-" + strings.ReplaceAll(spec.Name, ".", "")
	_ = exec.Command("docker", "rm", "-f", name).Run()
	args := []string{"run", "-d", "--name", name, "--rm"}
	if spec.Name == "5.7" && runtime.GOARCH == "arm64" {
		args = append(args, "--platform", "linux/amd64")
	}
	args = append(args,
		"-e", "MYSQL_ROOT_PASSWORD=flashback",
		"-e", "MYSQL_DATABASE=fbtest",
		"-p", fmt.Sprintf("127.0.0.1:%d:3306", spec.Port),
		spec.Image,
		"--log-bin=mysql-bin",
		"--binlog-format=ROW",
		"--binlog-row-image=FULL",
		"--server-id=1",
	)
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		return creds, nil, fmt.Errorf("docker run %s: %v: %s", spec.Image, err, strings.TrimSpace(string(out)))
	}
	cleanup = func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	}
	creds = flashbackMySQLCreds{Host: "127.0.0.1", Port: spec.Port, User: "root", Password: "flashback", DBName: "fbtest"}
	if err := flashbackWaitMySQL(flashbackMySQLMatrixDSN(spec.Port, "fbtest"), 3*time.Minute); err != nil {
		cleanup()
		return creds, nil, fmt.Errorf("mysql %s 未在 3 分钟内就绪: %w", spec.Name, err)
	}
	return creds, cleanup, nil
}

func flashbackWaitMySQL(dsn string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		db, err := sql.Open("mysql", dsn)
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
	return last
}

func flashbackLookupMySQLMatrixSpec(name string) (flashbackMySQLMatrixSpec, bool) {
	for _, s := range flashbackMySQLMatrixVersions {
		if s.Name == name {
			return s, true
		}
	}
	n, err := strconv.Atoi(strings.ReplaceAll(name, ".", ""))
	if err == nil {
		for _, s := range flashbackMySQLMatrixVersions {
			if strings.ReplaceAll(s.Name, ".", "") == strconv.Itoa(n) {
				return s, true
			}
		}
	}
	return flashbackMySQLMatrixSpec{}, false
}
