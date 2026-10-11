package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vocat/internal/store"
)

// 号码归属地：后端代理请求 uapis.cn，避免在前端暴露 API Key。
// API Key 仅通过网页「设置」保存到数据库（app_settings 表），不再读取本地配置文件。
const (
	phoneInfoSettingKey = "phone.info.api_key"
	phoneInfoEndpoint   = "https://uapis.cn/api/v1/misc/phoneinfo"
	phoneInfoCacheTTL   = 24 * time.Hour
	phoneInfoMaxBody    = 64 << 10
	phoneInfoMaxKeyLen  = 256
)

var phoneInfoHTTPClient = &http.Client{Timeout: 8 * time.Second}

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

// loadPhoneInfoAPIKey 从数据库加载归属地 API Key（启动时调用；设置页保存后也会即时刷新）。
func (s *Server) loadPhoneInfoAPIKey(ctx context.Context) {
	key := ""
	if setting, err := s.store.AppSetting(ctx, phoneInfoSettingKey); err == nil {
		var document struct {
			APIKey string `json:"api_key"`
		}
		if json.Unmarshal(setting.Value, &document) == nil {
			key = strings.TrimSpace(document.APIKey)
		}
	}
	s.applyPhoneInfoKey(key)
}

func (s *Server) applyPhoneInfoKey(key string) {
	s.phoneInfoMu.Lock()
	s.phoneInfoAPIKey = key
	s.phoneInfoCache = make(map[string]phoneInfoCacheEntry)
	s.phoneInfoMu.Unlock()
}

func (s *Server) phoneInfoKey() string {
	s.phoneInfoMu.RLock()
	defer s.phoneInfoMu.RUnlock()
	return s.phoneInfoAPIKey
}

func validatePhoneInfoKey(key string) error {
	if key == "" {
		return nil
	}
	if len(key) > phoneInfoMaxKeyLen {
		return errors.New("api key is too long")
	}
	if strings.ContainsAny(key, "\x00\r\n") {
		return errors.New("api key contains invalid characters")
	}
	return nil
}

// handlePhoneInfoSettings 读写归属地 API Key（设置页使用，不回传明文）。
//
//	GET /api/settings/phone-info  -> { configured }
//	PUT /api/settings/phone-info  <- { api_key }（空字符串表示清除）
func (s *Server) handlePhoneInfoSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writePhoneInfoSettings(w)
	case http.MethodPut:
		var request struct {
			APIKey *string `json:"api_key"`
		}
		if err := s.decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if request.APIKey == nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "api_key is required")
			return
		}
		key := strings.TrimSpace(*request.APIKey)
		if err := validatePhoneInfoKey(key); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_api_key", err.Error())
			return
		}
		if key == "" {
			if err := s.store.DeleteAppSetting(r.Context(), phoneInfoSettingKey); err != nil && !errors.Is(err, store.ErrNotFound) {
				s.writeStoreError(w, err)
				return
			}
		} else {
			value, err := json.Marshal(map[string]string{"api_key": key})
			if err != nil {
				s.writeStoreError(w, err)
				return
			}
			if err := s.store.UpsertAppSetting(r.Context(), store.AppSetting{
				Key:   phoneInfoSettingKey,
				Value: value,
			}); err != nil {
				s.writeStoreError(w, err)
				return
			}
		}
		s.applyPhoneInfoKey(key)
		s.recordAudit(r.Context(), "admin", "settings.phone_info.api_key", "settings", "phone-info", "success", "phone info api key updated")
		s.writePhoneInfoSettings(w)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *Server) writePhoneInfoSettings(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
		"configured": s.phoneInfoKey() != "",
	}})
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
