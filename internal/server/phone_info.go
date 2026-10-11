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
	"sync"
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
	phoneInfoBatchLimit = 100
	phoneInfoWorkers    = 5
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

// resolvePhoneLocation 依次从内存、数据库、上游接口解析一个号码的归属地。
// 解析成功写回数据库，保证同号只查一次上游；失败返回 ok=false。
func (s *Server) resolvePhoneLocation(ctx context.Context, raw string) (phoneInfoResult, bool) {
	key := phoneInfoLookupKey(raw)
	if key == "" {
		return phoneInfoResult{}, false
	}
	if cached, ok := s.cachedPhoneInfo(key); ok {
		return cached, true
	}
	if stored, err := s.store.PhoneLocation(ctx, key); err == nil &&
		(stored.Province != "" || stored.City != "" || stored.Sp != "") {
		result := phoneInfoResult{Province: stored.Province, City: stored.City, Sp: stored.Sp, AreaCode: stored.AreaCode}
		s.storePhoneInfo(key, result)
		return result, true
	}
	result, err := s.fetchPhoneInfo(ctx, key)
	if err != nil {
		return phoneInfoResult{}, false
	}
	_ = s.store.UpsertPhoneLocation(ctx, store.PhoneLocation{
		Number: key, Province: result.Province, City: result.City, Sp: result.Sp, AreaCode: result.AreaCode,
	})
	s.storePhoneInfo(key, result)
	return result, true
}

// handlePhoneInfo 返回单个号码归属地。
//
//	GET /api/phone-info?phone=13800138000
func (s *Server) handlePhoneInfo(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("phone"))
	if raw == "" {
		writeError(w, http.StatusBadRequest, "invalid_phone", "a valid phone number is required")
		return
	}
	result, ok := s.resolvePhoneLocation(r.Context(), raw)
	if !ok {
		writeError(w, http.StatusBadGateway, "phone_info_failed", "unable to resolve the phone location")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": phoneInfoPayload(raw, result)})
}

// handlePhoneInfoBatch 批量返回号码归属地（最多 100 个），缺失的号码直接省略。
//
//	GET /api/phone-info/batch?phones=13800138000,13900139000
func (s *Server) handlePhoneInfoBatch(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	phones := parsePhoneListParam(r)
	if len(phones) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_phone", "at least one phone number is required")
		return
	}
	items := s.resolvePhoneLocationsBatch(r.Context(), phones)
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"items": items}})
}

func parsePhoneListParam(r *http.Request) []string {
	seen := make(map[string]struct{})
	phones := make([]string, 0, phoneInfoBatchLimit)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		phones = append(phones, value)
	}
	for _, param := range r.URL.Query()["phones"] {
		for _, part := range strings.Split(param, ",") {
			add(part)
			if len(phones) >= phoneInfoBatchLimit {
				return phones
			}
		}
	}
	return phones
}

// resolvePhoneLocationsBatch 解析一批号码，返回存在归属地的号码（phone 字段为原始输入）。
func (s *Server) resolvePhoneLocationsBatch(ctx context.Context, phones []string) []map[string]any {
	resolved := make(map[string]phoneInfoResult, len(phones))
	missing := make([]string, 0, len(phones))
	for _, raw := range phones {
		key := phoneInfoLookupKey(raw)
		if key == "" {
			continue
		}
		if cached, ok := s.cachedPhoneInfo(key); ok {
			resolved[key] = cached
			continue
		}
		missing = append(missing, key)
	}
	if len(missing) > 0 {
		if stored, err := s.store.PhoneLocations(ctx, missing); err == nil {
			remaining := missing[:0]
			for _, key := range missing {
				loc, ok := stored[key]
				if ok && (loc.Province != "" || loc.City != "" || loc.Sp != "") {
					result := phoneInfoResult{Province: loc.Province, City: loc.City, Sp: loc.Sp, AreaCode: loc.AreaCode}
					resolved[key] = result
					s.storePhoneInfo(key, result)
				} else {
					remaining = append(remaining, key)
				}
			}
			missing = remaining
		}
	}
	// 并发请求上游，串行写库，避免 SQLite 写锁竞争。
	type fetched struct {
		key    string
		result phoneInfoResult
		ok     bool
	}
	fetchedList := make([]fetched, len(missing))
	sem := make(chan struct{}, phoneInfoWorkers)
	var wg sync.WaitGroup
	for index, key := range missing {
		wg.Add(1)
		go func(index int, key string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			result, err := s.fetchPhoneInfo(ctx, key)
			if err != nil {
				fetchedList[index] = fetched{key: key, ok: false}
				return
			}
			fetchedList[index] = fetched{key: key, result: result, ok: true}
		}(index, key)
	}
	wg.Wait()
	for _, item := range fetchedList {
		if !item.ok {
			continue
		}
		_ = s.store.UpsertPhoneLocation(ctx, store.PhoneLocation{
			Number: item.key, Province: item.result.Province, City: item.result.City, Sp: item.result.Sp, AreaCode: item.result.AreaCode,
		})
		resolved[item.key] = item.result
		s.storePhoneInfo(item.key, item.result)
	}
	// 按原始顺序输出存在归属地的号码。
	items := make([]map[string]any, 0, len(resolved))
	emitted := make(map[string]struct{}, len(resolved))
	for _, raw := range phones {
		key := phoneInfoLookupKey(raw)
		if key == "" {
			continue
		}
		if _, ok := emitted[key]; ok {
			continue
		}
		result, ok := resolved[key]
		if !ok {
			continue
		}
		emitted[key] = struct{}{}
		items = append(items, phoneInfoPayload(raw, result))
	}
	return items
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

// phoneInfoLookupKey 归一化为 11 位大陆手机号，用于查询与数据库存储；无法识别返回空串。
func phoneInfoLookupKey(value string) string {
	digits := strings.Builder{}
	for _, character := range value {
		if character >= '0' && character <= '9' {
			digits.WriteRune(character)
		}
	}
	normalized := digits.String()
	// 带国家码的 13 位号码（86 前缀）归一化为 11 位。
	if len(normalized) == 13 && strings.HasPrefix(normalized, "86") {
		normalized = normalized[2:]
	}
	if len(normalized) != 11 {
		return ""
	}
	return normalized
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
