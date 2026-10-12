package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"vocat/internal/auth"
	"vocat/internal/buildinfo"
	"vocat/internal/developer"
	"vocat/internal/i18n"
	"vocat/internal/loghub"
	"vocat/internal/store"
	"vocat/internal/update"
)

func (s *Server) routeGeneralAPI(w http.ResponseWriter, r *http.Request) bool {
	cleanPath := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api"), "/")
	if s.routeAutomaticTasksAPI(w, r, cleanPath) {
		return true
	}
	if s.routeExtensionAPI(w, r, cleanPath) {
		return true
	}
	if s.routeExportProxyAPI(w, r, cleanPath) {
		return true
	}
	if s.routeSMSAPI(w, r, cleanPath) {
		return true
	}
	if s.routeProxyAPI(w, r, cleanPath) {
		return true
	}
	if s.routeSettingsAPI(w, r, cleanPath) {
		return true
	}
	if cleanPath == "call-history" || strings.HasPrefix(cleanPath, "call-history/") {
		s.handleCallHistory(w, r, strings.TrimPrefix(strings.TrimPrefix(cleanPath, "call-history"), "/"))
		return true
	}
	if cleanPath == "contacts" || strings.HasPrefix(cleanPath, "contacts/") {
		s.handleContacts(w, r, strings.TrimPrefix(strings.TrimPrefix(cleanPath, "contacts"), "/"))
		return true
	}
	switch cleanPath {
	case "logs/history":
		s.handleLogHistory(w, r)
	case "logs/stream":
		s.handleLogStream(w, r)
	case "system/info":
		s.handleSystemInfo(w, r)
	case "system/update/check":
		s.handleUpdateCheck(w, r)
	case "system/update/apply":
		s.handleUpdateApply(w, r)
	case "settings/password":
		s.handlePasswordChange(w, r)
	case "settings/preferences":
		s.handleUIPreferences(w, r)
	case "settings/ringtone/custom":
		s.handleCustomRingtone(w, r)
	case "settings/https":
		s.handleHTTPSSettings(w, r)
	case "settings/https/certificate":
		s.handleHTTPSCertificate(w, r)
	case "settings/developer":
		s.handleDeveloperSettings(w, r)
	case "phone-info":
		s.handlePhoneInfo(w, r)
	case "phone-info/batch":
		s.handlePhoneInfoBatch(w, r)
	default:
		return false
	}
	return true
}

const uiPreferencesSettingKey = "ui.preferences"

const defaultRingtone = "classic"

// 预设来电铃声标识；custom 表示用户上传的自定义铃声。
var validRingtones = map[string]bool{
	"classic": true,
	"single":  true,
	"chime":   true,
	"beep":    true,
	"custom":  true,
}

type uiPreferencesDocument struct {
	Language string `json:"language,omitempty"`
	Ringtone string `json:"ringtone,omitempty"`
}

// loadUIPreferences 读取持久化的偏好文档；缺省 language=en、ringtone=classic。
func (s *Server) loadUIPreferences(ctx context.Context) uiPreferencesDocument {
	document := uiPreferencesDocument{Language: "en", Ringtone: defaultRingtone}
	setting, err := s.store.AppSetting(ctx, uiPreferencesSettingKey)
	if err == nil {
		_ = json.Unmarshal(setting.Value, &document)
	}
	if document.Language != "en" && document.Language != "zh" {
		document.Language = "en"
	}
	if !validRingtones[document.Ringtone] {
		document.Ringtone = defaultRingtone
	}
	return document
}

func (s *Server) saveUIPreferences(ctx context.Context, document uiPreferencesDocument) error {
	raw, err := json.Marshal(document)
	if err != nil {
		return err
	}
	return s.store.UpsertAppSetting(ctx, store.AppSetting{Key: uiPreferencesSettingKey, Value: raw})
}

// loadUILanguage primes the process-level UI language (internal/i18n) from the
// persisted preference so backend-generated strings are translated correctly
// even before the first preferences request arrives after a restart.
func (s *Server) loadUILanguage(ctx context.Context) {
	setting, err := s.store.AppSetting(ctx, uiPreferencesSettingKey)
	if err != nil {
		return
	}
	var document struct {
		Language string `json:"language"`
	}
	if json.Unmarshal(setting.Value, &document) == nil {
		i18n.Set(document.Language)
	}
}

// handleUIPreferences reads and writes UI preferences such as the interface
// language and incoming-call ringtone. Preferences live in the database so they
// stay consistent across the browsers and devices of the single administrator.
// PUT 采用合并写：仅更新请求中出现的字段，避免语言与铃声选择互冲。
func (s *Server) handleUIPreferences(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeUIPreferences(w, r)
	case http.MethodPut:
		var request struct {
			Language *string `json:"language"`
			Ringtone *string `json:"ringtone"`
		}
		if err := s.decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		document := s.loadUIPreferences(r.Context())
		if request.Language != nil {
			language := strings.ToLower(strings.TrimSpace(*request.Language))
			if language != "en" && language != "zh" {
				writeError(w, http.StatusBadRequest, "invalid_language", "language must be \"en\" or \"zh\"")
				return
			}
			document.Language = language
		}
		if request.Ringtone != nil {
			ringtone := strings.ToLower(strings.TrimSpace(*request.Ringtone))
			if !validRingtones[ringtone] {
				writeError(w, http.StatusBadRequest, "invalid_ringtone", "ringtone must be one of classic, single, chime, beep, custom")
				return
			}
			document.Ringtone = ringtone
		}
		if err := s.saveUIPreferences(r.Context(), document); err != nil {
			s.writeStoreError(w, err)
			return
		}
		s.writeUIPreferences(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *Server) writeUIPreferences(w http.ResponseWriter, r *http.Request) {
	document := s.loadUIPreferences(r.Context())
	// Keep the process-level UI language in sync so backend-generated strings
	// (status text, errors, hints) translate to match the SPA.
	i18n.Set(document.Language)
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{"language": document.Language, "ringtone": document.Ringtone},
	})
}

const customRingtoneSettingKey = "ui.ringtone.custom"

// 自定义铃声上传上限（与插件上传同走 multipart，不受 JSON 1MB 全局限制）。
const maxRingtoneUploadBytes = 8 << 20

// 允许上传的音频 MIME 类型（multipart 文件头 Content-Type）。
var validRingtoneMimeTypes = map[string]bool{
	"audio/mpeg": true,
	"audio/mp3":  true,
	"audio/ogg":  true,
	"audio/wav":  true,
	"audio/x-wav": true,
	"audio/mp4":  true,
	"audio/aac":  true,
	"audio/x-m4a": true,
	"audio/webm": true,
}

type customRingtoneMeta struct {
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
}

func (s *Server) loadCustomRingtoneMeta(ctx context.Context) (customRingtoneMeta, bool) {
	setting, err := s.store.AppSetting(ctx, customRingtoneSettingKey)
	if err != nil {
		return customRingtoneMeta{}, false
	}
	var meta customRingtoneMeta
	if json.Unmarshal(setting.Value, &meta) != nil || meta.MimeType == "" {
		return customRingtoneMeta{}, false
	}
	return meta, true
}

// handleCustomRingtone 管理用户上传的自定义来电铃声：文件本体存数据目录下
// 固定路径（s.ringtoneFile，docker 中即 /opt/vocat/data/ringtone），
// 原始文件名与 MIME 类型作为元数据存 AppSetting。
func (s *Server) handleCustomRingtone(w http.ResponseWriter, r *http.Request) {
	if s.ringtoneFile == "" {
		writeError(w, http.StatusServiceUnavailable, "ringtone_unavailable", "custom ringtone storage is not configured")
		return
	}
	switch r.Method {
	case http.MethodHead:
		// 探测是否已上传（前端设置页用），不读文件本体；文件名经 URL 编码放响应头。
		meta, ok := s.loadCustomRingtoneMeta(r.Context())
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "no custom ringtone uploaded")
			return
		}
		w.Header().Set("X-Ringtone-Name", url.QueryEscape(meta.Name))
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		meta, ok := s.loadCustomRingtoneMeta(r.Context())
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "no custom ringtone uploaded")
			return
		}
		data, err := os.ReadFile(s.ringtoneFile)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "no custom ringtone uploaded")
			return
		}
		w.Header().Set("Content-Type", meta.MimeType)
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, meta.Name, time.Time{}, bytes.NewReader(data))
	case http.MethodPut:
		r.Body = http.MaxBytesReader(nil, r.Body, maxRingtoneUploadBytes+(1<<20))
		if err := r.ParseMultipartForm(maxRingtoneUploadBytes); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_ringtone_upload", "ringtone upload must be multipart/form-data and no larger than 8 MiB")
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_ringtone_upload", "multipart field file is required")
			return
		}
		defer file.Close()
		mimeType := strings.ToLower(strings.TrimSpace(header.Header.Get("Content-Type")))
		if !validRingtoneMimeTypes[mimeType] {
			writeError(w, http.StatusBadRequest, "invalid_ringtone_type", "ringtone must be an audio file (mp3, ogg, wav, m4a, aac, webm)")
			return
		}
		data, err := io.ReadAll(io.LimitReader(file, maxRingtoneUploadBytes+1))
		if err != nil || len(data) == 0 || len(data) > maxRingtoneUploadBytes {
			writeError(w, http.StatusBadRequest, "invalid_ringtone_upload", "ringtone file is empty or larger than 8 MiB")
			return
		}
		if err := os.WriteFile(s.ringtoneFile, data, 0o600); err != nil {
			s.logger.Error("write custom ringtone file", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "an internal error occurred")
			return
		}
		name := strings.TrimSpace(filepath.Base(header.Filename))
		if name == "" || name == "." {
			name = "ringtone"
		}
		raw, _ := json.Marshal(customRingtoneMeta{Name: name, MimeType: mimeType})
		if err := s.store.UpsertAppSetting(r.Context(), store.AppSetting{Key: customRingtoneSettingKey, Value: raw}); err != nil {
			s.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": customRingtoneMeta{Name: name, MimeType: mimeType}})
	case http.MethodDelete:
		if err := os.Remove(s.ringtoneFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.logger.Warn("remove custom ringtone file", "error", err)
		}
		if err := s.store.DeleteAppSetting(r.Context(), customRingtoneSettingKey); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.writeStoreError(w, err)
			return
		}
		// 当前选择为自定义铃声时回退到默认预设。
		document := s.loadUIPreferences(r.Context())
		if document.Ringtone == "custom" {
			document.Ringtone = defaultRingtone
			if err := s.saveUIPreferences(r.Context(), document); err != nil {
				s.writeStoreError(w, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"deleted": true}})
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *Server) handleLogHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		clearedAt := time.Now().UTC()
		if s.logs != nil {
			s.logs.Clear()
		}
		deleted, err := s.store.ClearLogEvents(r.Context(), clearedAt)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{"cleared": true, "deleted": deleted},
		})
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	limit, err := strconv.Atoi(r.URL.Query().Get("lines"))
	if err != nil || limit < 1 {
		limit = 500
	}
	if limit > 2000 {
		limit = 2000
	}
	minimum := logLevel(r.URL.Query().Get("level"))
	search := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("search")))

	// History is served from the persisted log_events table so it reflects the
	// configured retention policy and survives restarts (the in-memory hub only
	// backs the live stream).
	entries := []loghub.Entry{}
	if s.store != nil {
		events, err := s.store.ListLogEvents(r.Context(), store.LogFilter{
			Limit: limit, ExcludeMessage: "http request",
		})
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		for _, event := range events {
			if storedLogLevel(event.Level) < minimum {
				continue
			}
			entry := loghub.SanitizeEntry(storedLogToEntry(event))
			if loghub.IsHTTPAccessEntry(entry) {
				continue
			}
			if search != "" && !storedLogContains(entry, search) {
				continue
			}
			entries = append(entries, entry)
			if len(entries) == limit {
				break
			}
		}
		// ListLogEvents is newest-first; present chronologically.
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{"logs": entries},
	})
}

func storedLogLevel(value string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "error":
		return slog.LevelError
	case "warn", "warning":
		return slog.LevelWarn
	case "debug":
		return slog.LevelDebug
	default:
		return slog.LevelInfo
	}
}

func storedLogToEntry(event store.LogEvent) loghub.Entry {
	var fields map[string]any
	if len(event.Fields) > 0 {
		if err := json.Unmarshal(event.Fields, &fields); err != nil {
			fields = nil
		}
	}
	return loghub.Entry{
		Time:    event.Time,
		Level:   event.Level,
		Message: event.Message,
		Caller:  event.Caller,
		Fields:  fields,
	}
}

func storedLogContains(entry loghub.Entry, search string) bool {
	if strings.Contains(strings.ToLower(entry.Message), search) ||
		strings.Contains(strings.ToLower(entry.Caller), search) {
		return true
	}
	for key, value := range entry.Fields {
		if strings.Contains(strings.ToLower(key), search) ||
			strings.Contains(strings.ToLower(fmt.Sprint(value)), search) {
			return true
		}
	}
	return false
}

func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if s.logs == nil {
		writeError(w, http.StatusServiceUnavailable, "log_stream_unavailable", "live log stream is unavailable")
		return
	}
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Time{}); err != nil {
		s.logger.Debug("stream write deadline is controlled by the HTTP server", "error", err)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("event: connected\ndata: {}\n\n")); err != nil {
		return
	}
	if err := controller.Flush(); err != nil {
		return
	}

	minimum := logLevel(r.URL.Query().Get("level"))
	entries, cancel := s.logs.Subscribe(128)
	defer cancel()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	encoder := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		case entry, ok := <-entries:
			if !ok {
				return
			}
			entry = loghub.SanitizeEntry(entry)
			if loghub.IsHTTPAccessEntry(entry) || logLevel(entry.Level) < minimum {
				continue
			}
			if _, err := w.Write([]byte("event: log\ndata: ")); err != nil {
				return
			}
			if err := encoder.Encode(entry); err != nil {
				return
			}
			if _, err := w.Write([]byte("\n")); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		}
	}
}

func logLevel(value string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "error":
		return slog.LevelError
	case "warn", "warning":
		return slog.LevelWarn
	case "debug", "all", "":
		return slog.LevelDebug
	default:
		return slog.LevelInfo
	}
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"version":      buildinfo.Version,
			"build_time":   buildinfo.BuildTime,
			"config":       "VOCAT_CONFIG and environment",
			"os":           runtime.GOOS,
			"architecture": runtime.GOARCH,
			"uptime":       formatDuration(time.Since(s.startedAt)),
			"developer":    s.developerActive(r.Context()),
		},
	})
}

func (s *Server) developerActive(ctx context.Context) bool {
	return s.developerEnabled && developer.Enabled(ctx, s.store)
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if strings.TrimSpace(s.updateRepository) == "" || s.updateCheck == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{
				"available": false,
				"version":   buildinfo.Version,
				"message":   i18n.T("未配置受信任的软件更新源；不会从未知地址下载或执行文件。"),
			},
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	result, err := s.updateCheck(
		ctx,
		s.updateRepository,
		s.updateToken,
		buildinfo.Version,
	)
	if err != nil {
		s.logger.Warn("check for updates failed", "repository", s.updateRepository, "error", err)
		writeError(w, http.StatusBadGateway, "update_check_failed", err.Error())
		return
	}
	message := ""
	if result.Available {
		message = result.ReleaseNotes
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"available":       result.Available,
			"current_version": result.Current,
			"version":         result.Latest,
			"message":         message,
			"repository":      s.updateRepository,
			"is_docker":       runningInDocker(),
		},
	})
}

func runningInDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("VOCAT_CONTAINER")), "docker")
}

func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if strings.TrimSpace(s.updateRepository) == "" || s.updateApply == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{
				"applied": false,
				"message": i18n.T("未配置受信任的软件更新源；未执行任何更新。"),
			},
		})
		return
	}
	if runningInDocker() {
		writeError(w, http.StatusConflict, "container_update_required", "pull the latest container image and recreate the container")
		return
	}
	s.updateMu.Lock()
	if s.updateApplying {
		s.updateMu.Unlock()
		writeError(w, http.StatusConflict, "update_busy", "another update is already in progress")
		return
	}
	s.updateApplying = true
	s.updateMu.Unlock()
	defer func() {
		s.updateMu.Lock()
		s.updateApplying = false
		s.updateMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	result, err := s.updateApply(ctx, s.logger, update.Options{
		Repo:  s.updateRepository,
		Token: s.updateToken,
	}, false)
	if err != nil {
		s.logger.Error("apply update failed", "repository", s.updateRepository, "error", err)
		writeError(w, http.StatusBadGateway, "update_apply_failed", err.Error())
		return
	}
	if !result.Applied {
		writeJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{
				"applied": false,
				"version": result.Latest,
				"message": "The installed version is already current.",
			},
		})
		return
	}
	// A binary update changes the trusted server code underneath every active
	// browser/API session. Revoke every durable token before scheduling the
	// restart and expire this client's cookies so all users must authenticate
	// against the newly installed version.
	if err := s.store.DeleteAllSessions(r.Context()); err != nil {
		s.logger.Error("revoke sessions after update failed", "error", err)
		writeError(w, http.StatusInternalServerError, "update_session_revocation_failed", "The update was installed, but active sessions could not be revoked; restart the service and sign in again.")
		return
	}
	s.clearAuthCookies(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"applied":                   true,
			"version":                   result.Latest,
			"reauthentication_required": true,
			"message":                   "Update verified and installed; all sessions were revoked and the service is restarting.",
		},
	})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	if s.updateRestart != nil {
		restart := s.updateRestart
		logger := s.logger
		go func() {
			time.Sleep(time.Second)
			if err := restart(logger); err != nil {
				logger.Error("restart after update failed", "error", err)
			}
		}()
	}
}

func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var request struct {
		OldPassword     string `json:"old_password"`
		NewPassword     string `json:"new_password"`
		ConfirmPassword string `json:"confirm_password"`
	}
	if err := s.decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if request.NewPassword != request.ConfirmPassword {
		writeError(w, http.StatusBadRequest, "password_mismatch", "new password and confirmation do not match")
		return
	}
	sessionToken, ok := s.sessionToken(w, r)
	if !ok {
		return
	}
	session, err := s.auth.Authenticate(r.Context(), sessionToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication is required")
		return
	}
	if err := s.auth.ChangePassword(
		r.Context(),
		session.Principal.Username,
		request.OldPassword,
		request.NewPassword,
	); err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials):
			writeError(w, http.StatusUnauthorized, "invalid_credentials", "current password is incorrect")
		case errors.Is(err, auth.ErrEmptyPassword):
			writeError(w, http.StatusBadRequest, "weak_password", err.Error())
		case strings.Contains(err.Error(), "must differ"):
			writeError(w, http.StatusBadRequest, "password_reused", err.Error())
		default:
			s.logger.Error("password change failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "an internal error occurred")
		}
		return
	}
	s.clearAuthCookies(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{"changed": true, "reauthentication_required": true},
	})
}

func formatDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	days := int(duration / (24 * time.Hour))
	duration %= 24 * time.Hour
	hours := int(duration / time.Hour)
	duration %= time.Hour
	minutes := int(duration / time.Minute)
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}
