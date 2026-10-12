package server

import (
	"net/http"

	"vocat/internal/sipgw"
)

// handleVoiceGatewaySettings 读写 CS 域语音网关（SIP/软电话桥接）配置。
func (s *Server) handleVoiceGatewaySettings(w http.ResponseWriter, r *http.Request) {
	if s.voiceGateway == nil {
		writeError(w, http.StatusNotImplemented, "voice_gateway_unavailable", "the CS voice gateway is not available in this build")
		return
	}
	switch r.Method {
	case http.MethodGet:
		config, err := s.voiceGateway.Redacted(r.Context())
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"config": config,
			"status": s.voiceGateway.Status(r.Context()),
		}})
	case http.MethodPut:
		var request sipgw.Config
		if err := s.decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if _, err := s.voiceGateway.Save(r.Context(), request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_voice_gateway", err.Error())
			return
		}
		config, err := s.voiceGateway.Redacted(r.Context())
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		s.recordAudit(r.Context(), "admin", "settings.voice_gateway.update", "settings", "voice-gateway", "success",
			"语音网关配置已更新")
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"config": config,
			"status": s.voiceGateway.Status(r.Context()),
		}})
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}
