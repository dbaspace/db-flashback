package service

import (
	"fmt"
	"net"
	"strings"

	mdmmodel "db-flashback/internal/mdmmodel"
)

var flashbackCloudHostFrags = []string{
	"rds.aliyuncs.com",
	"rds.aliyun.com",
	"polardb",
	"pg.rds.",
	"mysql.rds.",
	"tencentcdb.com",
	"postgres.tencentcloud",
	"cdb.tencent",
	"tencentcloudapi.com",
	"myqcloud.com",
	"huaweicloud.com",
	"rds.myhuaweicloud.com",
	"rds.amazonaws.com",
	"amazonaws.com",
	"azure.com",
	"database.azure.com",
	"aliyuncs.com",
}

var flashbackCloudTagKeys = []string{"cloud", "vendor", "deploy_type", "product", "engine_product"}

var flashbackCloudTagVals = []string{
	"aliyun", "aliyun_rds", "rds", "polardb", "tencent", "tencent_pg", "tencentdb",
	"huawei", "aws", "azure", "cloud",
}

var flashbackCloudRoles = []string{"rds_superuser", "pg_tencentdb_superuser", "rdsadmin"}

// flashbackCloudVersionFrags 云厂商发行版关键字（握手/@@version，如腾讯 TXSQL）。
var flashbackCloudVersionFrags = []string{
	"txsql", "tdsql", "cynosdb", "polardb", "aliyun", "tencentdb",
}

// flashbackLooksLikeCloudHost 根据地址判断是否为云厂商托管实例。
func flashbackLooksLikeCloudHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	if hostOnly, _, err := net.SplitHostPort(h); err == nil {
		h = strings.ToLower(hostOnly)
	}
	for _, frag := range flashbackCloudHostFrags {
		if strings.Contains(h, frag) {
			return true
		}
	}
	return false
}

func flashbackTagValue(tags map[string]interface{}, key string) string {
	if tags == nil {
		return ""
	}
	v, ok := tags[key]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.ToLower(strings.TrimSpace(t))
	case []string:
		if len(t) > 0 {
			return strings.ToLower(strings.TrimSpace(t[0]))
		}
	case []interface{}:
		if len(t) > 0 {
			return strings.ToLower(strings.TrimSpace(fmt.Sprint(t[0])))
		}
	default:
		return strings.ToLower(strings.TrimSpace(fmt.Sprint(t)))
	}
	return ""
}

// flashbackCloudReason 若判定为云库则返回中性原因（不含产品策略），否则返回空串。
func flashbackCloudReason(res *mdmmodel.ResourceDbsInfo, extraHosts ...string) string {
	if res != nil {
		if flashbackLooksLikeCloudHost(res.Address) {
			return fmt.Sprintf("实例地址 %s 匹配云厂商托管域名", res.Address)
		}
		if r := flashbackCloudVersionReason(res.Version); r != "" {
			return r
		}
		for _, k := range flashbackCloudTagKeys {
			val := flashbackTagValue(res.Tags, k)
			if val == "" {
				continue
			}
			for _, want := range flashbackCloudTagVals {
				if val == want || strings.Contains(val, want) {
					return fmt.Sprintf("实例标签 %s=%s 表明为云数据库", k, val)
				}
			}
		}
	}
	for _, h := range extraHosts {
		if flashbackLooksLikeCloudHost(h) {
			return fmt.Sprintf("连接地址 %s 匹配云厂商托管域名", h)
		}
	}
	return ""
}

// flashbackCloudVersionReason 根据版本串判断云厂商发行版（公司域名后面的 TXSQL/PolarDB 等）。
func flashbackCloudVersionReason(ver string) string {
	raw := strings.TrimSpace(ver)
	if raw == "" {
		return ""
	}
	low := strings.ToLower(raw)
	for _, frag := range flashbackCloudVersionFrags {
		if strings.Contains(low, frag) {
			return fmt.Sprintf("实例版本 %s 为云厂商发行版", raw)
		}
	}
	return ""
}

func flashbackCloudRoleReason(roleNames []string) string {
	set := map[string]struct{}{}
	for _, n := range roleNames {
		set[strings.ToLower(strings.TrimSpace(n))] = struct{}{}
	}
	for _, r := range flashbackCloudRoles {
		if _, ok := set[r]; ok {
			return fmt.Sprintf("目标库存在云厂商角色 %s", r)
		}
	}
	return ""
}

var flashbackTencentHostFrags = []string{
	"tencentcdb.com",
	"postgres.tencentcloud",
	"cdb.tencent",
}

func flashbackLooksLikeTencentHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	if hostOnly, _, err := net.SplitHostPort(h); err == nil {
		h = strings.ToLower(hostOnly)
	}
	for _, frag := range flashbackTencentHostFrags {
		if strings.Contains(h, frag) {
			return true
		}
	}
	return false
}
