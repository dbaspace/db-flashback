package service

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"

	"db-flashback/internal/config"
	mdmmodel "db-flashback/internal/mdmmodel"
	"db-flashback/internal/storage/databases/ent"
	"db-flashback/internal/storage/flashback"
)

// baseService 仅保留闪回锁测试用的空运行时；独立项目不用 Redis。
type flashbackRuntime struct {
	redisLock any
}

var baseService *flashbackRuntime

var runtimeCfg *config.SvrConfig

func InitRuntime(cfg *config.SvrConfig) {
	runtimeCfg = cfg
}

func runtimeConfig() *config.SvrConfig {
	if runtimeCfg != nil {
		return runtimeCfg
	}
	return config.TryGlobal()
}

func instanceRowToConfig(r flashback.InstanceRow) config.InstanceConfig {
	return config.InstanceConfig{
		ID: r.ID, DBType: r.DBType, Host: r.Host, Port: r.Port,
		User: r.User, Password: r.Password, SSLMode: r.SSLMode,
		Vendor: r.Vendor, CloudInstanceID: r.CloudInstanceID, Region: r.Region,
		SSHUser: r.SSHUser, SSHPort: r.SSHPort,
	}
}

func lookupConfiguredInstance(instanceID string) (config.InstanceConfig, error) {
	id := strings.TrimSpace(instanceID)
	if id == "" {
		return config.InstanceConfig{}, fmt.Errorf("instance_id 必填")
	}
	if row, err := flashbackStore.GetInstance(context.Background(), id); err == nil && row != nil {
		return instanceRowToConfig(*row), nil
	}
	return config.InstanceConfig{}, fmt.Errorf("instance not found: %s（请先在「实例地址」登记）", id)
}

func lookupConfiguredInstanceByHostPort(host string, port int) (config.InstanceConfig, error) {
	host = strings.TrimSpace(host)
	if host == "" || port <= 0 {
		return config.InstanceConfig{}, fmt.Errorf("instance not found")
	}
	if rows, err := flashbackStore.ListInstances(context.Background()); err == nil {
		var found []config.InstanceConfig
		for _, r := range rows {
			if strings.EqualFold(strings.TrimSpace(r.Host), host) && r.Port == port {
				found = append(found, instanceRowToConfig(r))
			}
		}
		if len(found) == 1 {
			return found[0], nil
		}
	}
	return config.InstanceConfig{}, fmt.Errorf("instance not found")
}

func instanceToDomain(inst config.InstanceConfig) *ent.DomainInstance {
	port := inst.Port
	if port <= 0 {
		if flashbackIsMySQL(inst.DBType) {
			port = defaultMySQLPort
		} else {
			port = 5432
		}
	}
	return &ent.DomainInstance{
		ID:         strings.TrimSpace(inst.ID),
		MainIP:     strings.TrimSpace(inst.Host),
		DomainName: strings.TrimSpace(inst.Host),
		InstanceID: strings.TrimSpace(inst.CloudInstanceID),
		DbType:     normalizeInstanceDBType(inst.DBType),
		Port:       port,
	}
}

func instanceToResource(inst config.InstanceConfig) *mdmmodel.ResourceDbsInfo {
	port := inst.Port
	if port <= 0 {
		if flashbackIsMySQL(inst.DBType) {
			port = defaultMySQLPort
		} else {
			port = 5432
		}
	}
	tags := map[string]interface{}{}
	for k, v := range inst.Tags {
		tags[k] = v
	}
	if v := strings.TrimSpace(inst.Vendor); v != "" {
		if _, ok := tags["flash_vendor"]; !ok {
			tags["flash_vendor"] = v
		}
	}
	if v := strings.TrimSpace(inst.CloudInstanceID); v != "" {
		if _, ok := tags["cloud_instance_id"]; !ok {
			tags["cloud_instance_id"] = v
		}
		if strings.HasPrefix(strings.ToLower(v), "postgres-") {
			if _, ok := tags["pg"]; !ok {
				tags["pg"] = v
			}
		}
	}
	if v := strings.TrimSpace(inst.Region); v != "" {
		if _, ok := tags["flash_region"]; !ok {
			tags["flash_region"] = v
		}
	}
	res := &mdmmodel.ResourceDbsInfo{
		Address:           net.JoinHostPort(strings.TrimSpace(inst.Host), strconv.Itoa(port)),
		Name:              strings.TrimSpace(inst.ID),
		Version:           strings.TrimSpace(inst.Version),
		Tags:              tags,
		DbType:            mdmmodel.DBTypeEnum(normalizeInstanceDBType(inst.DBType)),
		CredentialAccount: strings.TrimSpace(inst.User),
	}
	if r := strings.TrimSpace(inst.Region); r != "" {
		res.Region = &mdmmodel.Region{Name: r, Region: r}
		res.RegionID = r
	}
	return res
}

func normalizeInstanceDBType(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "mysql", "mariadb":
		return string(mdmmodel.MySQLDBTypeEnum)
	case "postgres", "postgresql", "pg":
		return string(mdmmodel.PostgreSQLDBTypeEnum)
	default:
		return s
	}
}

func openConfiguredMySQL(ctx context.Context, inst config.InstanceConfig, dbName string) (*sql.DB, flashbackMySQLCreds, error) {
	creds := flashbackMySQLCreds{
		Host:     strings.TrimSpace(inst.Host),
		Port:     inst.Port,
		User:     strings.TrimSpace(inst.User),
		Password: inst.Password,
		DBName:   strings.TrimSpace(dbName),
	}
	if creds.Port <= 0 {
		creds.Port = defaultMySQLPort
	}
	if creds.DBName == "" {
		creds.DBName = "mysql"
	}
	db, err := sql.Open("mysql", flashbackMySQLDSN(creds))
	if err != nil {
		return nil, creds, fmt.Errorf("sql.Open mysql: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		if flashbackMySQLUnknownDatabase(err) && !strings.EqualFold(creds.DBName, "mysql") {
			fallback := creds
			fallback.DBName = "mysql"
			db, err = sql.Open("mysql", flashbackMySQLDSN(fallback))
			if err != nil {
				return nil, creds, fmt.Errorf("sql.Open mysql: %w", err)
			}
			if perr := db.PingContext(ctx); perr != nil {
				_ = db.Close()
				return nil, creds, fmt.Errorf("ping mysql: %w", perr)
			}
			return db, creds, nil
		}
		return nil, creds, fmt.Errorf("ping mysql: %w", err)
	}
	return db, creds, nil
}

func openConfiguredPostgres(ctx context.Context, inst config.InstanceConfig, dbName string) (*sql.DB, error) {
	port := inst.Port
	if port <= 0 {
		port = 5432
	}
	ssl := strings.TrimSpace(inst.SSLMode)
	if ssl == "" {
		ssl = "disable"
	}
	if strings.TrimSpace(dbName) == "" {
		dbName = "postgres"
	}
	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		strings.TrimSpace(inst.Host), port, strings.TrimSpace(inst.User), inst.Password, dbName, ssl)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open postgres: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return db, nil
}

func connectSourcePG(ctx context.Context, instanceID, dbName, fallbackHost string, fallbackPort int) (*sql.DB, *mdmmodel.ResourceDbsInfo, error) {
	inst, err := resolveConnectInstance(instanceID, fallbackHost, fallbackPort)
	if err != nil {
		return nil, nil, err
	}
	res := instanceToResource(inst)
	db, err := openConfiguredPostgres(ctx, inst, dbName)
	if err != nil {
		return nil, res, err
	}
	return db, res, nil
}

func resolveConnectInstance(instanceID, fallbackHost string, fallbackPort int) (config.InstanceConfig, error) {
	if inst, err := lookupConfiguredInstance(instanceID); err == nil {
		return inst, nil
	}
	if inst, err := lookupConfiguredInstanceByHostPort(fallbackHost, fallbackPort); err == nil {
		return inst, nil
	}
	return config.InstanceConfig{}, fmt.Errorf("instance not found: %s", strings.TrimSpace(instanceID))
}

func getGlobalArgIntDefault(ctx context.Context, key string, fallback int) int {
	if v := strings.TrimSpace(lookupFlashbackArg(ctx, key)); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func getGlobalArgStrDefault(ctx context.Context, key, fallback string) string {
	if v := strings.TrimSpace(lookupFlashbackArg(ctx, key)); v != "" {
		return v
	}
	return fallback
}

func lookupFlashbackArg(ctx context.Context, key string) string {
	if ctx == nil {
		ctx = context.Background()
	}
	if v, err := flashbackStore.GetArg(ctx, key); err == nil && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	envKey := strings.ToUpper(strings.TrimSpace(key))
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v
	}
	cfg := runtimeConfig()
	if cfg == nil {
		return ""
	}
	return cfg.Flashback.Arg(key)
}

func lookupFlashbackArgSource(ctx context.Context, key string) (value, source string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if v, err := flashbackStore.GetArg(ctx, key); err == nil && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), "db"
	}
	envKey := strings.ToUpper(strings.TrimSpace(key))
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v, "env"
	}
	cfg := runtimeConfig()
	if cfg != nil {
		if v := strings.TrimSpace(cfg.Flashback.Arg(key)); v != "" {
			return v, "yaml"
		}
	}
	return "", ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func resolveCdcOperator(c *gin.Context) string {
	if name := CurrentUsername(c); name != "" {
		return name
	}
	if c == nil {
		return "system"
	}
	for _, key := range []string{"X-User-Name", "X-Username", "X-User", "X-Forwarded-User"} {
		if name := strings.TrimSpace(c.GetHeader(key)); name != "" {
			return name
		}
	}
	return "system"
}
