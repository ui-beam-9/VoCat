package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 号码归属地：后端代理请求 uapis.cn，避免在前端暴露 API Key。
// API Key 存放于数据目录下的 phone-info.json，配置项见 phoneInfoConfig。
const (
	phoneInfoConfigFile = "phone-info.json"
	phoneInfoEndpoint   = "https://uapis.cn/api/v1/misc/phoneinfo"
	phoneInfoCacheTTL   = 24 * time.Hour
	phoneInfoMaxBody    = 64 << 10
)

var phoneInfoHTTPClient = &http.Client{Timeout: 8 * time.Second}

type phoneInfoConfig struct {
	APIKey string `json:"api_key"`
}

type phoneInfoResult struct {
	Province string `json:"province"`
	City     string `json:"city"`
	Sp       string `json:"sp"`
	AreaCode string `json:"area_code"`
}

type phoneInfoCacheEntry struct {
	result    phoneInfoResult
	fetchedAt time.Time
}

// loadPhoneInfoConfig 读取数据目录下的 phone-info.json；文件缺失或损坏时返回空配置，
// 不影响服务启动（接口仍可依赖访客积分访问）。
func loadPhoneInfoConfig(dataDir string) phoneInfoConfig {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return phoneInfoConfig{}
	}
	path := filepath.Join(dataDir, phoneInfoConfigFile)
	file, err := os.Open(path)
	if err != nil {
		return phoneInfoConfig{}
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, phoneInfoMaxBody))
	var config phoneInfoConfig
	if err := decoder.Decode(&config); err != nil {
		return phoneInfoConfig{}
	}
	config.APIKey = strings.TrimSpace(config.APIKey)
	return config
}

func (s *Server) phoneInfoKey() string {
	s.phoneInfoMu.RLock()
	defer s.phoneInfoMu.RUnlock()
	return s.phoneInfoAPIKey
}

// handlePhoneInfo 返回号码归属地。
//
//	GET /api/phone-info?phone=13800138000
func (s *Server) handlePhoneInfo(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	phone := normalizePhoneQuery(r.URL.Query().Get("phone"))
	if phone == "" {
		writeError(w, http.StatusBadRequest, "invalid_phone", "a valid phone number is required")
		return
	}
	if cached, ok := s.cachedPhoneInfo(phone); ok {
		writeJSON(w, http.StatusOK, map[string]any{"data": phoneInfoPayload(phone, cached)})
		return
	}
	result, err := s.fetchPhoneInfo(r.Context(), phone)
	if err != nil {
		s.logger.Warn("phone info lookup failed", "category", "call", "event", "phone_info.lookup", "phone", phone, "error", err)
		writeError(w, http.StatusBadGateway, "phone_info_failed", err.Error())
		return
	}
	s.storePhoneInfo(phone, result)
	writeJSON(w, http.StatusOK, map[string]any{"data": phoneInfoPayload(phone, result)})
}

func (s *Server) fetchPhoneInfo(ctx context.Context, phone string) (phoneInfoResult, error) {
	endpoint := phoneInfoEndpoint + "?phone=" + url.QueryEscape(phone)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return phoneInfoResult{}, err
	}
	request.Header.Set("Accept", "application/json")
	if key := s.phoneInfoKey(); key != "" {
		request.Header.Set("Authorization", key)
	}
	response, err := phoneInfoHTTPClient.Do(request)
	if err != nil {
		return phoneInfoResult{}, fmt.Errorf("请求归属地接口失败：%w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, phoneInfoMaxBody))
	if err != nil {
		return phoneInfoResult{}, fmt.Errorf("读取归属地接口响应失败：%w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return phoneInfoResult{}, fmt.Errorf("归属地接口返回状态 %d", response.StatusCode)
	}
	return parsePhoneInfoBody(body)
}

func parsePhoneInfoBody(body []byte) (phoneInfoResult, error) {
	var result phoneInfoResult
	if err := json.Unmarshal(body, &result); err != nil {
		return phoneInfoResult{}, errors.New("归属地接口返回内容无法解析")
	}
	if result.Province == "" && result.City == "" && result.Sp == "" {
		return phoneInfoResult{}, errors.New("未查询到该号码的归属地信息")
	}
	return result, nil
}

func phoneInfoPayload(phone string, result phoneInfoResult) map[string]any {
	return map[string]any{
		"phone":     phone,
		"province":  result.Province,
		"city":      result.City,
		"sp":        result.Sp,
		"area_code": result.AreaCode,
		"display":   phoneInfoDisplay(result),
	}
}

// phoneInfoDisplay 拼接展示文案，例如「北京 移动」。
func phoneInfoDisplay(result phoneInfoResult) string {
	location := strings.TrimSpace(result.Province)
	if city := strings.TrimSpace(result.City); city != "" && city != location {
		if location == "" {
			location = city
		} else {
			location = location + " " + city
		}
	}
	sp := strings.TrimSpace(result.Sp)
	switch {
	case location != "" && sp != "":
		return location + " " + sp
	case location != "":
		return location
	default:
		return sp
	}
}

// normalizePhoneQuery 仅保留数字并允许首位 +，返回用于查询的号码。
func normalizePhoneQuery(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	digits := strings.Builder{}
	for index, character := range value {
		switch {
		case character >= '0' && character <= '9':
			digits.WriteRune(character)
		case index == 0 && character == '+':
			digits.WriteRune('+')
		}
	}
	result := digits.String()
	if len(result) < 5 || len(result) > 20 {
		return ""
	}
	return result
}

func (s *Server) cachedPhoneInfo(phone string) (phoneInfoResult, bool) {
	s.phoneInfoMu.RLock()
	defer s.phoneInfoMu.RUnlock()
	entry, ok := s.phoneInfoCache[phone]
	if !ok || time.Since(entry.fetchedAt) > phoneInfoCacheTTL {
		return phoneInfoResult{}, false
	}
	return entry.result, true
}

func (s *Server) storePhoneInfo(phone string, result phoneInfoResult) {
	s.phoneInfoMu.Lock()
	defer s.phoneInfoMu.Unlock()
	if s.phoneInfoCache == nil {
		s.phoneInfoCache = make(map[string]phoneInfoCacheEntry)
	}
	s.phoneInfoCache[phone] = phoneInfoCacheEntry{result: result, fetchedAt: time.Now()}
}
