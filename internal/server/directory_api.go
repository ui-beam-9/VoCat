package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vocat/internal/store"
)

func callHistoryPayload(entry store.CallHistoryEntry) map[string]any {
	return map[string]any{
		"id":               entry.ID,
		"call_id":          entry.CallID,
		"device_id":        entry.DeviceID,
		"number":           entry.Number,
		"direction":        entry.Direction,
		"outcome":          entry.Outcome,
		"started_at":       entry.StartedAt.Format(time.RFC3339),
		"ended_at":         entry.EndedAt.Format(time.RFC3339),
		"duration_seconds": entry.DurationSeconds,
	}
}

func contactPayload(value store.Contact) map[string]any {
	return map[string]any{
		"id":         value.ID,
		"name":       value.Name,
		"number":     value.Number,
		"created_at": value.CreatedAt.Format(time.RFC3339),
		"updated_at": value.UpdatedAt.Format(time.RFC3339),
	}
}

func parseRFC3339Seconds(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// handleCallHistory serves the server-side VoWiFi call log:
//
//	GET    /api/call-history?device_id=&limit=  list newest first
//	POST   /api/call-history                    append one finished call
//	DELETE /api/call-history?device_id=         clear (all devices when empty)
//	DELETE /api/call-history/{id}               remove one entry
func (s *Server) handleCallHistory(w http.ResponseWriter, r *http.Request, id string) {
	if id != "" {
		s.handleCallHistoryEntry(w, r, id)
		return
	}
	switch r.Method {
	case http.MethodGet:
		limit := 200
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 1000 {
				limit = parsed
			}
		}
		entries, err := s.store.ListCallHistory(r.Context(), r.URL.Query().Get("device_id"), limit)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		items := make([]map[string]any, 0, len(entries))
		for _, entry := range entries {
			items = append(items, callHistoryPayload(entry))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{"entries": items, "total": len(items)},
		})
	case http.MethodPost:
		var request struct {
			CallID          string `json:"call_id"`
			DeviceID        string `json:"device_id"`
			Number          string `json:"number"`
			Direction       string `json:"direction"`
			Outcome         string `json:"outcome"`
			StartedAt       string `json:"started_at"`
			EndedAt         string `json:"ended_at"`
			DurationSeconds int    `json:"duration_seconds"`
		}
		if err := s.decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		entry := store.CallHistoryEntry{
			CallID:          request.CallID,
			DeviceID:        request.DeviceID,
			Number:          request.Number,
			Direction:       request.Direction,
			Outcome:         request.Outcome,
			DurationSeconds: request.DurationSeconds,
		}
		if parsed, ok := parseRFC3339Seconds(request.StartedAt); ok {
			entry.StartedAt = parsed
		}
		if parsed, ok := parseRFC3339Seconds(request.EndedAt); ok {
			entry.EndedAt = parsed
		}
		stored, err := s.store.AppendCallHistory(r.Context(), entry)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"data": map[string]any{"entry": callHistoryPayload(stored)},
		})
	case http.MethodDelete:
		if err := s.store.ClearCallHistory(r.Context(), r.URL.Query().Get("device_id")); err != nil {
			s.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"cleared": true}})
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

// handleCallHistoryEntry removes a single call log entry.
//
//	DELETE /api/call-history/{id}
func (s *Server) handleCallHistoryEntry(w http.ResponseWriter, r *http.Request, rawID string) {
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(rawID), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_id", "call history id is invalid")
		return
	}
	if err := s.store.DeleteCallHistoryEntry(r.Context(), id); err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"deleted": true, "id": id}})
}

// handleContacts serves the saved address book:
//
//	GET  /api/contacts  list
//	POST /api/contacts  create or rename one contact (keyed by number)
func (s *Server) handleContacts(w http.ResponseWriter, r *http.Request, id string) {
	if id != "" {
		s.handleContactEntry(w, r, id)
		return
	}
	switch r.Method {
	case http.MethodGet:
		values, err := s.store.ListContacts(r.Context())
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		items := make([]map[string]any, 0, len(values))
		for _, value := range values {
			items = append(items, contactPayload(value))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{"entries": items, "total": len(items)},
		})
	case http.MethodPost:
		var request struct {
			Name   string `json:"name"`
			Number string `json:"number"`
		}
		if err := s.decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		stored, err := s.store.UpsertContact(r.Context(), store.Contact{
			Name:   request.Name,
			Number: request.Number,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"data": map[string]any{"entry": contactPayload(stored)},
		})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

// handleContactEntry edits or removes one contact.
//
//	PUT    /api/contacts/{id}  update name/number
//	DELETE /api/contacts/{id}  remove
func (s *Server) handleContactEntry(w http.ResponseWriter, r *http.Request, rawID string) {
	id, err := strconv.ParseInt(strings.TrimSpace(rawID), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_id", "contact id is invalid")
		return
	}
	switch r.Method {
	case http.MethodPut:
		var request struct {
			Name   string `json:"name"`
			Number string `json:"number"`
		}
		if err := s.decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		stored, err := s.store.UpdateContact(r.Context(), id, request.Name, request.Number)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found", "contact not found")
				return
			}
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{"entry": contactPayload(stored)},
		})
	case http.MethodDelete:
		if err := s.store.DeleteContact(r.Context(), id); err != nil {
			s.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"deleted": true, "id": id}})
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}
