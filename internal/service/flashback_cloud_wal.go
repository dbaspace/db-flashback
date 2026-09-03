package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	mdmmodel "db-flashback/internal/mdmmodel"
	"db-flashback/internal/storage/databases/ent"
)

const (
	flashbackWALSourceSelf          = "self"
	flashbackWALSourceCloudTencent  = "cloud_tencent"
	flashbackWALSourceCloudOther    = "cloud_other"
	flashbackWALSourceTencentNeedID = "cloud_tencent_need_id"

	flashbackVendorTencent = "tencent"
	flashbackVendorAliyun  = "aliyun"
	flashbackVendorHuawei  = "huawei"
	flashbackVendorAWS     = "aws"

	gaFlashbackTencentSecretID       = "flashback_tencent_secret_id"
	gaFlashbackTencentSecretKey      = "flashback_tencent_secret_key"
	gaFlashbackAliyunAccessKeyID     = "flashback_aliyun_access_key_id"
	gaFlashbackAliyunAccessKeySecret = "flashback_aliyun_access_key_secret"
	gaFlashbackHuaweiAccessKeyID     = "flashback_huawei_access_key_id"
	gaFlashbackHuaweiSecretAccessKey = "flashback_huawei_secret_access_key"
	gaFlashbackAWSAccessKeyID        = "flashback_aws_access_key_id"
	gaFlashbackAWSSecretAccessKey    = "flashback_aws_secret_access_key"
	gaFlashbackTencentRegion         = "flashback_tencent_region"
	gaFlashbackTencentRegionMap      = "flashback_tencent_region_map"
	gaFlashbackCloudLookbackHours    = "flashback_cloud_wal_lookback_hours"
	gaFlashbackCloudDownloadMbps     = "flashback_cloud_download_mbps"
	gaFlashbackCloudAPIIntervalMS    = "flashback_cloud_api_interval_ms"
	gaFlashbackCloudMaxPackages      = "flashback_cloud_max_packages"
	gaFlashbackCloudPkgRetries       = "flashback_cloud_pkg_retries"

	flashbackDefaultCloudLookbackHours = 2
	flashbackDefaultCloudDownloadMbps  = 16
	flashbackDefaultCloudAPIIntervalMS = 400
	flashbackDefaultCloudMaxPackages   = 48
	flashbackDefaultCloudPkgRetries    = 3

	// flashbackCloudOSSLag 云实例把 WAL 传到对象存储的滞后。下载窗开始减、结束加；解析仍用用户时间窗。
	flashbackCloudOSSLag = 3 * time.Minute
)

var flashbackTencentPGIDRe = regexp.MustCompile(`(?i)postgres-[a-z0-9]+`)

type flashbackWALSource struct {
	Kind       string // self / cloud_tencent / cloud_other / cloud_tencent_need_id
	Reason     string
	InstanceID string // postgres-xxxx
	Vendor     string
}

type flashbackCloudWALObject struct {
	ID         string
	Name       string
	Size       int64
	StartTime  time.Time
	FinishTime time.Time
	State      string
}

type flashbackCloudWALSpec struct {
	InstanceID string
	From       time.Time
	To         time.Time
}

type flashbackCloudWALProvider interface {
	Vendor() string
	ListByTime(ctx context.Context, spec flashbackCloudWALSpec) ([]flashbackCloudWALObject, error)
	DownloadURL(ctx context.Context, spec flashbackCloudWALSpec, obj flashbackCloudWALObject) (string, error)
}

func flashbackResolveWALSource(res *mdmmodel.ResourceDbsInfo, override string, extraHosts ...string) flashbackWALSource {
	if res != nil {
		if raw := flashbackTagValue(res.Tags, "flash_vendor"); raw != "" {
			vendor := flashbackNormalizeFlashVendor(raw)
			if vendor == "" {
				return flashbackWALSource{
					Kind: flashbackWALSourceCloudOther,
					Reason: fmt.Sprintf("MDM标签 flash_vendor=%s 不是规范值 tencent/aliyun/huawei/aws",
						strings.TrimSpace(flashbackTagRaw(res.Tags, "flash_vendor"))),
				}
			}
			return flashbackWALSourceForVendor(res, override, vendor, "MDM标签 flash_vendor="+vendor)
		}
	}
	if id, why := flashbackFindTencentPGInstanceID(res, override); id != "" {
		return flashbackWALSource{
			Kind: flashbackWALSourceCloudTencent, Reason: why, InstanceID: id, Vendor: flashbackVendorTencent,
		}
	}
	hosts := append([]string{}, extraHosts...)
	if res != nil {
		hosts = append(hosts, res.Address)
	}
	tencentHost := false
	otherVendor := ""
	otherCloud := ""
	for _, h := range hosts {
		if flashbackLooksLikeTencentHost(h) {
			tencentHost = true
			continue
		}
		if v := flashbackVendorFromCloudHost(h); v != "" {
			otherVendor = v
			otherCloud = fmt.Sprintf("连接地址 %s 匹配云厂商托管域名", strings.TrimSpace(h))
			continue
		}
		if flashbackLooksLikeCloudHost(h) {
			otherCloud = fmt.Sprintf("连接地址 %s 匹配云厂商托管域名", strings.TrimSpace(h))
		}
	}
	if res != nil {
		if r := flashbackCloudVersionReason(res.Version); r != "" && !strings.Contains(strings.ToLower(r), "txsql") {
			// txsql 是 MySQL 发行版，PG 路径不据此判厂商。
			if strings.Contains(strings.ToLower(r), "polardb") || strings.Contains(strings.ToLower(r), "aliyun") {
				otherVendor = flashbackVendorAliyun
				otherCloud = r
			}
		}
		for _, k := range flashbackCloudTagKeys {
			val := flashbackTagValue(res.Tags, k)
			if val == "" {
				continue
			}
			if v := flashbackNormalizeFlashVendor(val); v == flashbackVendorTencent || strings.Contains(val, "tencent") {
				tencentHost = true
				continue
			}
			if v := flashbackNormalizeFlashVendor(val); v != "" {
				otherVendor = v
				otherCloud = fmt.Sprintf("实例标签 %s=%s 表明为云数据库", k, val)
				continue
			}
			for _, want := range []string{"aliyun", "polardb", "huawei", "aws", "azure"} {
				if val == want || strings.Contains(val, want) {
					otherVendor = flashbackNormalizeFlashVendor(want)
					otherCloud = fmt.Sprintf("实例标签 %s=%s 表明为云数据库", k, val)
				}
			}
		}
	}
	if tencentHost {
		return flashbackWALSource{
			Kind: flashbackWALSourceTencentNeedID, Vendor: flashbackVendorTencent,
			Reason: "识别为腾讯云 PostgreSQL，但 MDM 未标注云产品 ID，请打标签 pg:postgres-xxxx",
		}
	}
	if otherCloud != "" {
		return flashbackWALSource{
			Kind: flashbackWALSourceCloudOther, Vendor: otherVendor,
			Reason: otherCloud + "，该云厂商 PostgreSQL 日志下载尚未实现",
		}
	}
	return flashbackWALSource{Kind: flashbackWALSourceSelf, Reason: "未识别为云厂商托管库"}
}

func flashbackWALSourceForVendor(res *mdmmodel.ResourceDbsInfo, override, vendor, why string) flashbackWALSource {
	if vendor == flashbackVendorTencent {
		if id, idWhy := flashbackFindTencentPGInstanceID(res, override); id != "" {
			return flashbackWALSource{
				Kind: flashbackWALSourceCloudTencent, Reason: why + "；" + idWhy,
				InstanceID: id, Vendor: flashbackVendorTencent,
			}
		}
		return flashbackWALSource{
			Kind: flashbackWALSourceTencentNeedID, Vendor: flashbackVendorTencent,
			Reason: why + "，但 MDM 未标注云产品 ID，请打标签 pg:postgres-xxxx",
		}
	}
	return flashbackWALSource{
		Kind: flashbackWALSourceCloudOther, Vendor: vendor,
		Reason: why + "，该云厂商 PostgreSQL 日志下载尚未实现",
	}
}

func flashbackNormalizeFlashVendor(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case flashbackVendorTencent, "tencent_pg", "tencentdb", "tencent_cloud":
		return flashbackVendorTencent
	case flashbackVendorAliyun, "aliyun_rds", "alibaba", "polardb":
		return flashbackVendorAliyun
	case flashbackVendorHuawei, "huaweicloud":
		return flashbackVendorHuawei
	case flashbackVendorAWS, "amazon":
		return flashbackVendorAWS
	default:
		return ""
	}
}

func flashbackVendorFromCloudHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return ""
	}
	if flashbackLooksLikeTencentHost(h) {
		return flashbackVendorTencent
	}
	for _, frag := range []string{"rds.aliyuncs.com", "rds.aliyun.com", "polardb", "aliyuncs.com"} {
		if strings.Contains(h, frag) {
			return flashbackVendorAliyun
		}
	}
	if strings.Contains(h, "huaweicloud") {
		return flashbackVendorHuawei
	}
	if strings.Contains(h, "amazonaws.com") {
		return flashbackVendorAWS
	}
	return ""
}

func flashbackFindTencentPGInstanceID(res *mdmmodel.ResourceDbsInfo, override string) (id, reason string) {
	if id := flashbackNormalizeTencentPGID(override); id != "" {
		return id, "任务入参 cloud_instance_id=" + id
	}
	if res == nil {
		return "", ""
	}
	if id := flashbackNormalizeTencentPGID(flashbackTagRaw(res.Tags, "pg")); id != "" {
		return id, "MDM标签 pg=" + id
	}
	for _, k := range []string{"cloud_instance_id", "db_instance_id", "tencent_instance_id"} {
		if id := flashbackNormalizeTencentPGID(flashbackTagRaw(res.Tags, k)); id != "" {
			return id, "MDM标签 " + k + "=" + id
		}
	}
	if res.Tags != nil {
		for k, v := range res.Tags {
			raw := strings.TrimSpace(fmt.Sprint(v))
			if id := flashbackNormalizeTencentPGID(raw); id != "" {
				return id, "MDM标签 " + k + "=" + id
			}
			if id := flashbackNormalizeTencentPGID(k); id != "" {
				return id, "MDM标签键 " + id
			}
		}
	}
	return "", ""
}

func flashbackNormalizeTencentPGID(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	m := flashbackTencentPGIDRe.FindString(s)
	if m == "" {
		return ""
	}
	return strings.ToLower(m)
}

func flashbackTagRaw(tags map[string]interface{}, key string) string {
	if tags == nil {
		return ""
	}
	v, ok := tags[key]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case []string:
		if len(t) > 0 {
			return strings.TrimSpace(t[0])
		}
	case []interface{}:
		if len(t) > 0 {
			return strings.TrimSpace(fmt.Sprint(t[0]))
		}
	default:
		return strings.TrimSpace(fmt.Sprint(t))
	}
	return ""
}

func flashbackResolveWALSourceFromConn(res *mdmmodel.ResourceDbsInfo, dom *ent.DomainInstance, override string) flashbackWALSource {
	var extra []string
	if dom != nil {
		extra = append(extra, dom.MainIP, dom.DomainName)
	}
	return flashbackResolveWALSource(res, override, extra...)
}

func flashbackCloudLookback(ctx context.Context) time.Duration {
	n := getGlobalArgIntDefault(ctx, gaFlashbackCloudLookbackHours, flashbackDefaultCloudLookbackHours)
	if n < 0 {
		n = flashbackDefaultCloudLookbackHours
	}
	return time.Duration(n) * time.Hour
}

func flashbackCloudDownloadMbps(ctx context.Context) int {
	n := getGlobalArgIntDefault(ctx, gaFlashbackCloudDownloadMbps, flashbackDefaultCloudDownloadMbps)
	if n < 0 {
		return 0
	}
	return n
}

func flashbackCloudAPIInterval(ctx context.Context) time.Duration {
	n := getGlobalArgIntDefault(ctx, gaFlashbackCloudAPIIntervalMS, flashbackDefaultCloudAPIIntervalMS)
	if n < 0 {
		n = flashbackDefaultCloudAPIIntervalMS
	}
	return time.Duration(n) * time.Millisecond
}

func flashbackCloudMaxPackages(ctx context.Context) int {
	n := getGlobalArgIntDefault(ctx, gaFlashbackCloudMaxPackages, flashbackDefaultCloudMaxPackages)
	if n <= 0 {
		return flashbackDefaultCloudMaxPackages
	}
	return n
}

func flashbackCloudPkgRetries(ctx context.Context) int {
	n := getGlobalArgIntDefault(ctx, gaFlashbackCloudPkgRetries, flashbackDefaultCloudPkgRetries)
	if n <= 0 {
		return 1
	}
	return n
}

// flashbackCloudVendorCreds 按厂商码读 global_args 对照表中的两把密钥。
// MDM 只提供 Vendor，不存键名；tencent→flashback_tencent_secret_id/flashback_tencent_secret_key，以此类推。
func flashbackCloudVendorCreds(ctx context.Context, vendor string) (id, key string, err error) {
	idKey, keyKey, _, _, err := flashbackCloudVendorKeyPair(vendor)
	if err != nil {
		return "", "", err
	}
	id = flashbackSafeGlobalArg(ctx, idKey)
	key = flashbackSafeGlobalArg(ctx, keyKey)
	if id == "" || key == "" {
		return "", "", fmt.Errorf("未配置该厂商密钥，请在控制台「多云」页保存 %s / %s", idKey, keyKey)
	}
	return id, key, nil
}

func flashbackCloudVendorKeyPair(vendor string) (idKey, keyKey, envID, envKey string, err error) {
	switch strings.ToLower(strings.TrimSpace(vendor)) {
	case flashbackVendorTencent:
		return gaFlashbackTencentSecretID, gaFlashbackTencentSecretKey,
			"FLASHBACK_TENCENT_SECRET_ID", "FLASHBACK_TENCENT_SECRET_KEY", nil
	case flashbackVendorAliyun:
		return gaFlashbackAliyunAccessKeyID, gaFlashbackAliyunAccessKeySecret,
			"FLASHBACK_ALIYUN_ACCESS_KEY_ID", "FLASHBACK_ALIYUN_ACCESS_KEY_SECRET", nil
	case flashbackVendorHuawei:
		return gaFlashbackHuaweiAccessKeyID, gaFlashbackHuaweiSecretAccessKey,
			"FLASHBACK_HUAWEI_ACCESS_KEY_ID", "FLASHBACK_HUAWEI_SECRET_ACCESS_KEY", nil
	case flashbackVendorAWS:
		return gaFlashbackAWSAccessKeyID, gaFlashbackAWSSecretAccessKey,
			"FLASHBACK_AWS_ACCESS_KEY_ID", "FLASHBACK_AWS_SECRET_ACCESS_KEY", nil
	default:
		return "", "", "", "", fmt.Errorf("未知云厂商 %q，无法匹配 global_args", vendor)
	}
}

func flashbackSafeGlobalArg(ctx context.Context, key string) (out string) {
	defer func() { _ = recover() }()
	return strings.TrimSpace(getGlobalArgStrDefault(ctx, key, ""))
}

func flashbackTencentRegion(ctx context.Context) string {
	if v := strings.TrimSpace(os.Getenv("FLASHBACK_TENCENT_REGION")); v != "" {
		return v
	}
	return flashbackSafeGlobalArg(ctx, gaFlashbackTencentRegion)
}

func flashbackCloudDownloadWindow(target, end, checkpointTime time.Time, lookback time.Duration) (time.Time, time.Time) {
	start := target
	if !checkpointTime.IsZero() && checkpointTime.Before(start) {
		start = checkpointTime
	}
	if lookback > 0 {
		start = start.Add(-lookback)
	}
	if end.IsZero() {
		end = time.Now()
	}
	return start, end
}

func flashbackCloudApplyDownloadLag(from, to time.Time, lag time.Duration) (time.Time, time.Time) {
	if lag <= 0 {
		return from, to
	}
	if !from.IsZero() {
		from = from.Add(-lag)
	}
	if to.IsZero() {
		to = time.Now()
	}
	return from, to.Add(lag)
}

func flashbackCloudWaitDownloadLag(ctx context.Context, logicalEnd time.Time, lag time.Duration) error {
	if lag <= 0 {
		return nil
	}
	if logicalEnd.IsZero() {
		logicalEnd = time.Now()
	}
	wait := time.Until(logicalEnd.Add(lag))
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func flashbackCloudPkgOverlaps(pkg flashbackCloudWALObject, from, to time.Time) bool {
	if pkg.StartTime.IsZero() && pkg.FinishTime.IsZero() {
		return true
	}
	start := pkg.StartTime
	finish := pkg.FinishTime
	if start.IsZero() {
		start = finish
	}
	if finish.IsZero() {
		finish = start
	}
	return start.Before(to) && finish.After(from)
}

func flashbackFilterCloudWALPkgs(pkgs []flashbackCloudWALObject, from, to time.Time) []flashbackCloudWALObject {
	var out []flashbackCloudWALObject
	for _, p := range pkgs {
		if !strings.EqualFold(strings.TrimSpace(p.State), "finished") {
			continue
		}
		if flashbackCloudPkgOverlaps(p, from, to) {
			out = append(out, p)
		}
	}
	return out
}

func flashbackCloudPkgsBytes(pkgs []flashbackCloudWALObject) int64 {
	var n int64
	for _, p := range pkgs {
		n += p.Size
	}
	return n
}

func flashbackCloudPkgsSpan(pkgs []flashbackCloudWALObject) (from, to time.Time) {
	for _, p := range pkgs {
		if !p.StartTime.IsZero() && (from.IsZero() || p.StartTime.Before(from)) {
			from = p.StartTime
		}
		if !p.FinishTime.IsZero() && (to.IsZero() || p.FinishTime.After(to)) {
			to = p.FinishTime
		}
	}
	return from, to
}

type flashbackTencentRegionResolved struct {
	Region string
	Reason string
}

var flashbackBuiltinRegionMap = map[string]string{
	"bj": "ap-beijing", "beijing": "ap-beijing",
	"sh": "ap-shanghai", "shanghai": "ap-shanghai",
	"gz": "ap-guangzhou", "guangzhou": "ap-guangzhou",
	"sz": "ap-shenzhen", "shenzhen": "ap-shenzhen",
	"cd": "ap-chengdu", "chengdu": "ap-chengdu",
	"cq": "ap-chongqing", "chongqing": "ap-chongqing",
	"hz": "ap-hangzhou", "hangzhou": "ap-hangzhou",
	"hk": "ap-hongkong", "hongkong": "ap-hongkong", "hong-kong": "ap-hongkong",
	"sg": "ap-singapore", "singapore": "ap-singapore",
}

var flashbackRegionSkip = map[string]struct{}{
	"dev": {}, "test": {}, "prod": {}, "production": {}, "staging": {},
	"uat": {}, "qa": {}, "pre": {}, "prd": {},
}

func flashbackTencentRegionMap(ctx context.Context) map[string]string {
	out := map[string]string{}
	for k, v := range flashbackBuiltinRegionMap {
		out[k] = v
	}
	raw := flashbackSafeGlobalArg(ctx, gaFlashbackTencentRegionMap)
	if raw == "" {
		return out
	}
	var extra map[string]string
	if err := json.Unmarshal([]byte(raw), &extra); err != nil {
		return out
	}
	for k, v := range extra {
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.ToLower(strings.TrimSpace(v))
		if k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	return out
}

func flashbackNormalizeAPIRegion(raw string, extra map[string]string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, "_", "-")
	if s == "" {
		return ""
	}
	if _, skip := flashbackRegionSkip[s]; skip {
		return ""
	}
	if strings.HasPrefix(s, "ap-") || strings.HasPrefix(s, "eu-") || strings.HasPrefix(s, "na-") {
		return s
	}
	if extra != nil {
		if v := strings.TrimSpace(extra[s]); v != "" {
			return strings.ToLower(v)
		}
	}
	if v := flashbackBuiltinRegionMap[s]; v != "" {
		return v
	}
	return ""
}

func flashbackResolveTencentRegion(ctx context.Context, res *mdmmodel.ResourceDbsInfo, override string) (flashbackTencentRegionResolved, error) {
	extra := flashbackTencentRegionMap(ctx)
	try := func(raw, why string) (flashbackTencentRegionResolved, bool) {
		if r := flashbackNormalizeAPIRegion(raw, extra); r != "" {
			return flashbackTencentRegionResolved{Region: r, Reason: why}, true
		}
		return flashbackTencentRegionResolved{}, false
	}
	if res != nil {
		if got, ok := try(flashbackTagRaw(res.Tags, "flash_region"), "MDM标签 flash_region="+strings.TrimSpace(flashbackTagRaw(res.Tags, "flash_region"))); ok {
			return got, nil
		}
		for _, k := range []string{"tencent_region", "qcloud_region", "tc_region"} {
			if got, ok := try(flashbackTagRaw(res.Tags, k), "MDM标签 "+k+"="+strings.TrimSpace(flashbackTagRaw(res.Tags, k))); ok {
				return got, nil
			}
		}
		if res.Region != nil {
			if got, ok := try(res.Region.Region, "MDM Region.Region="+res.Region.Region); ok && strings.HasPrefix(got.Region, "ap-") && strings.EqualFold(strings.TrimSpace(res.Region.Region), got.Region) {
				return got, nil
			}
		}
		if got, ok := try(flashbackTagRaw(res.Tags, "idc"), "MDM标签 idc="+strings.TrimSpace(flashbackTagRaw(res.Tags, "idc"))); ok {
			return got, nil
		}
		if res.Region != nil {
			if got, ok := try(res.Region.Name, "MDM Region.Name="+res.Region.Name); ok {
				return got, nil
			}
			if got, ok := try(res.Region.Region, "MDM Region.Region="+res.Region.Region); ok {
				return got, nil
			}
		}
		if got, ok := try(res.RegionID, "MDM RegionID="+res.RegionID); ok {
			return got, nil
		}
		if res.Zone != nil {
			if got, ok := try(res.Zone.Name, "MDM Zone.Name="+res.Zone.Name); ok {
				return got, nil
			}
			if got, ok := try(res.Zone.Zone, "MDM Zone="+res.Zone.Zone); ok {
				return got, nil
			}
		}
		if got, ok := try(flashbackTagRaw(res.Tags, "region"), "MDM标签 region="+strings.TrimSpace(flashbackTagRaw(res.Tags, "region"))); ok {
			return got, nil
		}
	}
	if got, ok := try(override, "任务入参 cloud_region="+strings.TrimSpace(override)); ok {
		return got, nil
	}
	if got, ok := try(flashbackTencentRegion(ctx), "全局参数/环境变量 "+gaFlashbackTencentRegion); ok {
		return got, nil
	}
	return flashbackTencentRegionResolved{}, fmt.Errorf("无法解析腾讯云 Region，请在 MDM 打标签 flash_region=ap-xxxx（按云平台机房，如北京 ap-beijing）")
}

type flashbackRateLimitReader struct {
	r    io.Reader
	rate int64
}

func flashbackLimitReader(r io.Reader, bytesPerSec int64) io.Reader {
	if r == nil || bytesPerSec <= 0 {
		return r
	}
	return &flashbackRateLimitReader{r: r, rate: bytesPerSec}
}

func (r *flashbackRateLimitReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 && r.rate > 0 {
		d := time.Duration(float64(n) / float64(r.rate) * float64(time.Second))
		if d > time.Millisecond {
			time.Sleep(d)
		}
	}
	return n, err
}
