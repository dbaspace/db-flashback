package config

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	"github.com/spf13/viper"

	"db-flashback/pkg/utils/log"
)

type Config interface {
	Parse() error
}

func Init(cfg Config, content string) error {
	viper.SetConfigType("yaml")
	if err := viper.ReadConfig(strings.NewReader(content)); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return errors.Wrap(err, "fatal error config file")
		}
	}
	if err := viper.Unmarshal(cfg); err != nil {
		return errors.Wrap(err, "failed to unmarshal config")
	}
	return cfg.Parse()
}

type HTTPConfig struct {
	RunMode         string     `json:"run_mode" yaml:"run_mode" mapstructure:"run_mode"`
	Host            string     `json:"host" yaml:"host" mapstructure:"host"`
	Port            int        `json:"port" yaml:"port" mapstructure:"port"`
	ReadTimeoutSec  int        `json:"read_timeout_sec" yaml:"read_timeout_sec" mapstructure:"read_timeout_sec"`
	WriteTimeoutSec int        `json:"write_timeout_sec" yaml:"write_timeout_sec" mapstructure:"write_timeout_sec"`
	Log             log.Config `json:"log" yaml:"log" mapstructure:"log"`
}

func (c *HTTPConfig) Init() error {
	if c.Host == "" {
		c.Host = "0.0.0.0"
	}
	if c.Port == 0 {
		c.Port = 8620
	}
	if c.ReadTimeoutSec == 0 {
		c.ReadTimeoutSec = 10
	}
	if c.WriteTimeoutSec == 0 {
		c.WriteTimeoutSec = 1800
	}
	if c.RunMode == "" {
		c.RunMode = gin.ReleaseMode
	}
	log.Init(c.Log)
	return nil
}

type DBConfig struct {
	Host     string `json:"host" yaml:"host" mapstructure:"host"`
	Port     int    `json:"port" yaml:"port" mapstructure:"port"`
	User     string `json:"user" yaml:"user" mapstructure:"user"`
	Password string `json:"password" yaml:"password" mapstructure:"password"`
	DBName   string `json:"dbname" yaml:"dbname" mapstructure:"dbname"`
	SSLMode  string `json:"sslmode" yaml:"sslmode" mapstructure:"sslmode"`
}

func (c DBConfig) DSN() string {
	port := c.Port
	if port <= 0 {
		port = 5432
	}
	ssl := strings.TrimSpace(c.SSLMode)
	if ssl == "" {
		ssl = "disable"
	}
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		c.Host, port, c.User, c.Password, c.DBName, ssl)
}

type InstanceConfig struct {
	ID              string            `json:"id" yaml:"id" mapstructure:"id"`
	DBType          string            `json:"db_type" yaml:"db_type" mapstructure:"db_type"`
	Host            string            `json:"host" yaml:"host" mapstructure:"host"`
	Port            int               `json:"port" yaml:"port" mapstructure:"port"`
	User            string            `json:"user" yaml:"user" mapstructure:"user"`
	Password        string            `json:"password" yaml:"password" mapstructure:"password"`
	SSLMode         string            `json:"sslmode" yaml:"sslmode" mapstructure:"sslmode"`
	Vendor          string            `json:"vendor" yaml:"vendor" mapstructure:"vendor"`
	CloudInstanceID string            `json:"cloud_instance_id" yaml:"cloud_instance_id" mapstructure:"cloud_instance_id"`
	Region          string            `json:"region" yaml:"region" mapstructure:"region"`
	Version         string            `json:"version" yaml:"version" mapstructure:"version"`
	Tags            map[string]string `json:"tags" yaml:"tags" mapstructure:"tags"`
	SSHUser         string            `json:"ssh_user" yaml:"ssh_user" mapstructure:"ssh_user"`
	SSHPort         int               `json:"ssh_port" yaml:"ssh_port" mapstructure:"ssh_port"`
}

type FlashbackSettings struct {
	WorkDir    string `json:"workdir" yaml:"workdir" mapstructure:"workdir"`
	ArchiveDir string `json:"archive_dir" yaml:"archive_dir" mapstructure:"archive_dir"`
	SSHUser    string `json:"ssh_user" yaml:"ssh_user" mapstructure:"ssh_user"`
	SSHPort    int    `json:"ssh_port" yaml:"ssh_port" mapstructure:"ssh_port"`
	// OfflineRoot 离线 PDU 的 PGDATA/WAL 副本根目录，默认 {workdir}/offline。
	OfflineRoot           string   `json:"offline_root" yaml:"offline_root" mapstructure:"offline_root"`
	OfflineAllowPaths     []string `json:"offline_allow_paths" yaml:"offline_allow_paths" mapstructure:"offline_allow_paths"`
	MaxWALBytes           int      `json:"max_wal_bytes" yaml:"max_wal_bytes" mapstructure:"max_wal_bytes"`
	MaxSQLs               int      `json:"max_sqls" yaml:"max_sqls" mapstructure:"max_sqls"`
	MaxFPWPages           int      `json:"max_fpw_pages" yaml:"max_fpw_pages" mapstructure:"max_fpw_pages"`
	RunLockWaitSec        int      `json:"run_lock_wait_sec" yaml:"run_lock_wait_sec" mapstructure:"run_lock_wait_sec"`
	CloudLookbackHours    int      `json:"cloud_wal_lookback_hours" yaml:"cloud_wal_lookback_hours" mapstructure:"cloud_wal_lookback_hours"`
	CloudDownloadMbps     int      `json:"cloud_download_mbps" yaml:"cloud_download_mbps" mapstructure:"cloud_download_mbps"`
	CloudAPIIntervalMS    int      `json:"cloud_api_interval_ms" yaml:"cloud_api_interval_ms" mapstructure:"cloud_api_interval_ms"`
	CloudMaxPackages      int      `json:"cloud_max_packages" yaml:"cloud_max_packages" mapstructure:"cloud_max_packages"`
	CloudPkgRetries       int      `json:"cloud_pkg_retries" yaml:"cloud_pkg_retries" mapstructure:"cloud_pkg_retries"`
	TencentSecretID       string   `json:"tencent_secret_id" yaml:"tencent_secret_id" mapstructure:"tencent_secret_id"`
	TencentSecretKey      string   `json:"tencent_secret_key" yaml:"tencent_secret_key" mapstructure:"tencent_secret_key"`
	TencentRegion         string   `json:"tencent_region" yaml:"tencent_region" mapstructure:"tencent_region"`
	TencentRegionMap      string   `json:"tencent_region_map" yaml:"tencent_region_map" mapstructure:"tencent_region_map"`
	AliyunAccessKeyID     string   `json:"aliyun_access_key_id" yaml:"aliyun_access_key_id" mapstructure:"aliyun_access_key_id"`
	AliyunAccessKeySecret string   `json:"aliyun_access_key_secret" yaml:"aliyun_access_key_secret" mapstructure:"aliyun_access_key_secret"`
	HuaweiAccessKeyID     string   `json:"huawei_access_key_id" yaml:"huawei_access_key_id" mapstructure:"huawei_access_key_id"`
	HuaweiSecretAccessKey string   `json:"huawei_secret_access_key" yaml:"huawei_secret_access_key" mapstructure:"huawei_secret_access_key"`
	AWSAccessKeyID        string   `json:"aws_access_key_id" yaml:"aws_access_key_id" mapstructure:"aws_access_key_id"`
	AWSSecretAccessKey    string   `json:"aws_secret_access_key" yaml:"aws_secret_access_key" mapstructure:"aws_secret_access_key"`
	// DataKey 32 字节 hex/base64。默认为空，第一次启动自动生成并写回配置；环境变量 FLASHBACK_DATA_KEY 优先。
	DataKey string `json:"data_key" yaml:"data_key" mapstructure:"data_key"`
	// Args 多云参数兜底。
	Args map[string]string `json:"args" yaml:"args" mapstructure:"args"`
}

func (s FlashbackSettings) Arg(key string) string {
	key = strings.TrimSpace(key)
	if v := strings.TrimSpace(s.Args[key]); v != "" {
		return v
	}
	switch key {
	case "flashback_workdir":
		return s.WorkDir
	case "flashback_archive_dir":
		return s.ArchiveDir
	case "flashback_max_wal_bytes":
		return intString(s.MaxWALBytes)
	case "flashback_max_sqls":
		return intString(s.MaxSQLs)
	case "flashback_max_fpw_pages":
		return intString(s.MaxFPWPages)
	case "flashback_run_lock_wait_sec":
		return intString(s.RunLockWaitSec)
	case "flashback_cloud_wal_lookback_hours":
		return intString(s.CloudLookbackHours)
	case "flashback_cloud_download_mbps":
		return intString(s.CloudDownloadMbps)
	case "flashback_cloud_api_interval_ms":
		return intString(s.CloudAPIIntervalMS)
	case "flashback_cloud_max_packages":
		return intString(s.CloudMaxPackages)
	case "flashback_cloud_pkg_retries":
		return intString(s.CloudPkgRetries)
	case "flashback_tencent_secret_id":
		return s.TencentSecretID
	case "flashback_tencent_secret_key":
		return s.TencentSecretKey
	case "flashback_tencent_region":
		return s.TencentRegion
	case "flashback_tencent_region_map":
		return s.TencentRegionMap
	case "flashback_aliyun_access_key_id":
		return s.AliyunAccessKeyID
	case "flashback_aliyun_access_key_secret":
		return s.AliyunAccessKeySecret
	case "flashback_huawei_access_key_id":
		return s.HuaweiAccessKeyID
	case "flashback_huawei_secret_access_key":
		return s.HuaweiSecretAccessKey
	case "flashback_aws_access_key_id":
		return s.AWSAccessKeyID
	case "flashback_aws_secret_access_key":
		return s.AWSSecretAccessKey
	default:
		return ""
	}
}

func intString(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", n)
}

type SvrConfig struct {
	HTTP      HTTPConfig        `json:"http" yaml:"http" mapstructure:"http"`
	DB        DBConfig          `json:"db" yaml:"db" mapstructure:"db"`
	Flashback FlashbackSettings `json:"flashback" yaml:"flashback" mapstructure:"flashback"`
}

func (s *SvrConfig) Parse() error {
	if err := s.HTTP.Init(); err != nil {
		return err
	}
	if strings.TrimSpace(s.DB.Host) == "" {
		return fmt.Errorf("db.host is empty")
	}
	if strings.TrimSpace(s.DB.DBName) == "" {
		return fmt.Errorf("db.dbname is empty")
	}
	if s.DB.Port <= 0 {
		s.DB.Port = 5432
	}
	global = s
	return nil
}

var global *SvrConfig

func Global() *SvrConfig {
	if global == nil {
		panic("config not initialized")
	}
	return global
}

func TryGlobal() *SvrConfig {
	return global
}
