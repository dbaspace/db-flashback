package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	flashbackTencentAPIHost    = "postgres.tencentcloudapi.com"
	flashbackTencentAPIVersion = "2017-03-12"
	flashbackTencentAPIService = "postgres"
	flashbackTencentTimeLayout = "2006-01-02 15:04:05"
)

type flashbackTencentWALProvider struct {
	secretID  string
	secretKey string
	region    string
	interval  time.Duration
	client    *http.Client
	do        func(*http.Request) (*http.Response, error)

	mu       sync.Mutex
	lastCall time.Time
}

func flashbackNewTencentWALProvider(ctx context.Context, region string) (*flashbackTencentWALProvider, error) {
	sid, skey, err := flashbackCloudVendorCreds(ctx, flashbackVendorTencent)
	if err != nil {
		return nil, err
	}
	region = strings.TrimSpace(region)
	if region == "" {
		return nil, fmt.Errorf("腾讯云 Region 为空，请在 MDM 打标签 flash_region=ap-xxxx")
	}
	p := &flashbackTencentWALProvider{
		secretID:  sid,
		secretKey: skey,
		region:    region,
		interval:  flashbackCloudAPIInterval(ctx),
		client:    &http.Client{Timeout: 45 * time.Second},
	}
	p.do = p.client.Do
	return p, nil
}

func (p *flashbackTencentWALProvider) Vendor() string { return "tencent" }

func (p *flashbackTencentWALProvider) throttle(ctx context.Context) error {
	if p == nil || p.interval <= 0 {
		return nil
	}
	p.mu.Lock()
	wait := p.interval - time.Since(p.lastCall)
	p.mu.Unlock()
	if wait <= 0 {
		p.mu.Lock()
		p.lastCall = time.Now()
		p.mu.Unlock()
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		p.mu.Lock()
		p.lastCall = time.Now()
		p.mu.Unlock()
		return nil
	}
}

func (p *flashbackTencentWALProvider) ListByTime(ctx context.Context, spec flashbackCloudWALSpec) ([]flashbackCloudWALObject, error) {
	if strings.TrimSpace(spec.InstanceID) == "" {
		return nil, fmt.Errorf("腾讯云 DBInstanceId 为空")
	}
	minT := flashbackTencentFormatTime(spec.From)
	maxT := flashbackTencentFormatTime(spec.To)
	var all []flashbackCloudWALObject
	offset := 0
	const limit = 100
	for {
		if err := p.throttle(ctx); err != nil {
			return nil, err
		}
		body := map[string]any{
			"MinFinishTime": minT,
			"MaxFinishTime": maxT,
			"Filters": []map[string]any{{
				"Name":   "db-instance-id",
				"Values": []string{spec.InstanceID},
			}},
			"Limit":       limit,
			"Offset":      offset,
			"OrderBy":     "StartTime",
			"OrderByType": "asc",
		}
		raw, err := p.call(ctx, "DescribeLogBackups", body)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Response struct {
				TotalCount   int `json:"TotalCount"`
				LogBackupSet []struct {
					Id           string `json:"Id"`
					DBInstanceId string `json:"DBInstanceId"`
					StartTime    string `json:"StartTime"`
					FinishTime   string `json:"FinishTime"`
					Size         int64  `json:"Size"`
					Name         string `json:"Name"`
					State        string `json:"State"`
				} `json:"LogBackupSet"`
				Error *struct {
					Code    string `json:"Code"`
					Message string `json:"Message"`
				} `json:"Error"`
				RequestId string `json:"RequestId"`
			} `json:"Response"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("DescribeLogBackups 解析失败: %w", err)
		}
		if resp.Response.Error != nil && resp.Response.Error.Code != "" {
			return nil, fmt.Errorf("DescribeLogBackups: %s %s", resp.Response.Error.Code, resp.Response.Error.Message)
		}
		for _, it := range resp.Response.LogBackupSet {
			all = append(all, flashbackCloudWALObject{
				ID:         strings.TrimSpace(it.Id),
				Name:       strings.TrimSpace(it.Name),
				Size:       it.Size,
				StartTime:  flashbackTencentParseTime(it.StartTime),
				FinishTime: flashbackTencentParseTime(it.FinishTime),
				State:      strings.TrimSpace(it.State),
			})
		}
		if len(resp.Response.LogBackupSet) == 0 || len(all) >= resp.Response.TotalCount || len(resp.Response.LogBackupSet) < limit {
			break
		}
		offset += limit
	}
	return flashbackFilterCloudWALPkgs(all, spec.From, spec.To), nil
}

func (p *flashbackTencentWALProvider) DownloadURL(ctx context.Context, spec flashbackCloudWALSpec, obj flashbackCloudWALObject) (string, error) {
	if strings.TrimSpace(obj.ID) == "" {
		return "", fmt.Errorf("BackupId 为空")
	}
	if err := p.throttle(ctx); err != nil {
		return "", err
	}
	body := map[string]any{
		"DBInstanceId":  spec.InstanceID,
		"BackupType":    "LogBackup",
		"BackupId":      obj.ID,
		"URLExpireTime": 2,
		"BackupDownloadRestriction": map[string]any{
			"RestrictionType": "NONE",
		},
	}
	raw, err := p.call(ctx, "DescribeBackupDownloadURL", body)
	if err != nil {
		return "", err
	}
	var resp struct {
		Response struct {
			BackupDownloadURL string `json:"BackupDownloadURL"`
			Error             *struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Error"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("DescribeBackupDownloadURL 解析失败: %w", err)
	}
	if resp.Response.Error != nil && resp.Response.Error.Code != "" {
		return "", fmt.Errorf("DescribeBackupDownloadURL: %s %s", resp.Response.Error.Code, resp.Response.Error.Message)
	}
	u := strings.TrimSpace(resp.Response.BackupDownloadURL)
	if u == "" {
		return "", fmt.Errorf("DescribeBackupDownloadURL 返回空链接")
	}
	return u, nil
}

func (p *flashbackTencentWALProvider) call(ctx context.Context, action string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+flashbackTencentAPIHost+"/", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	ts := time.Now().UTC()
	hdr := flashbackTC3Headers(p.secretID, p.secretKey, flashbackTencentAPIService, flashbackTencentAPIHost, action, flashbackTencentAPIVersion, p.region, body, ts)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	do := p.do
	if do == nil {
		if p.client == nil {
			p.client = &http.Client{Timeout: 45 * time.Second}
		}
		do = p.client.Do
	}
	resp, err := do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s HTTP %d: %s", action, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func flashbackTC3Headers(secretID, secretKey, service, host, action, version, region string, payload []byte, ts time.Time) map[string]string {
	if ts.IsZero() {
		ts = time.Now().UTC()
	} else {
		ts = ts.UTC()
	}
	timestamp := fmt.Sprintf("%d", ts.Unix())
	date := ts.Format("2006-01-02")
	hashedPayload := sha256Hex(payload)
	canonicalHeaders := "content-type:application/json\nhost:" + host + "\n"
	signedHeaders := "content-type;host"
	canonicalRequest := "POST\n/\n\n" + canonicalHeaders + "\n" + signedHeaders + "\n" + hashedPayload
	credentialScope := date + "/" + service + "/tc3_request"
	stringToSign := "TC3-HMAC-SHA256\n" + timestamp + "\n" + credentialScope + "\n" + sha256Hex([]byte(canonicalRequest))
	secretDate := hmacSHA256([]byte("TC3"+secretKey), date)
	secretService := hmacSHA256(secretDate, service)
	secretSigning := hmacSHA256(secretService, "tc3_request")
	sig := hex.EncodeToString(hmacSHA256(secretSigning, stringToSign))
	auth := "TC3-HMAC-SHA256 Credential=" + secretID + "/" + credentialScope + ", SignedHeaders=" + signedHeaders + ", Signature=" + sig
	return map[string]string{
		"Content-Type":   "application/json",
		"Host":           host,
		"X-TC-Action":    action,
		"X-TC-Version":   version,
		"X-TC-Timestamp": timestamp,
		"X-TC-Region":    region,
		"Authorization":  auth,
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(msg))
	return m.Sum(nil)
}

func flashbackTencentFormatTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.In(flashbackTimeLocation()).Format(flashbackTencentTimeLayout)
}

func flashbackTencentParseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	loc := flashbackTimeLocation()
	if tm, err := time.ParseInLocation(flashbackTencentTimeLayout, s, loc); err == nil {
		return tm
	}
	if tm, err := time.Parse(time.RFC3339, s); err == nil {
		return tm
	}
	return time.Time{}
}

func flashbackDownloadHTTP(ctx context.Context, url, dest string, bytesPerSec int64) (int64, error) {
	if strings.TrimSpace(url) == "" {
		return 0, fmt.Errorf("下载地址为空")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("下载 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	tmp := dest + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, flashbackLimitReader(resp.Body, bytesPerSec))
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return n, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return n, closeErr
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return n, err
	}
	return n, nil
}

func flashbackCloudDownloadDestName(obj flashbackCloudWALObject, url string) string {
	base := filepath.Base(strings.TrimSpace(obj.Name))
	base = strings.ReplaceAll(base, "*", "x")
	if base == "" || base == "." || base == "/" {
		base = obj.ID
	}
	if !strings.Contains(base, ".") {
		u := strings.ToLower(url)
		switch {
		case strings.Contains(u, ".tar.zst"):
			base += ".tar.zst"
		case strings.Contains(u, ".tar.gz"), strings.Contains(u, ".tgz"):
			base += ".tar.gz"
		default:
			base += ".bin"
		}
	}
	return base
}
