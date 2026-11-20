package servers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
)

type HTTPHandler struct {
	svc  *ServerService
	repo Repository
}

func NewHTTPHandler(svc *ServerService, repo Repository) *HTTPHandler {
	return &HTTPHandler{svc: svc, repo: repo}
}

// ServeServers dispatches /api/v1/servers and /api/v1/servers/{uuid}.
func (h *HTTPHandler) ServeServers(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/v1/servers")
	trimmed = strings.Trim(trimmed, "/")

	if trimmed == "" {
		switch r.Method {
		case http.MethodGet:
			list, err := h.repo.ListByTeam(r.Context(), principal.SessionTeamID)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, list)
		case http.MethodPost:
			var input CreateServerInput
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				writeJSONError(w, http.StatusUnprocessableEntity, "invalid request body")
				return
			}
			if input.TeamID == 0 && principal.SessionTeamID != 0 {
				input.TeamID = principal.SessionTeamID
			}
			srv, err := h.svc.Create(r.Context(), principal, input)
			if err != nil {
				writeDomainErr(w, err)
				return
			}
			writeJSON(w, http.StatusCreated, srv)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	uuid := strings.Split(trimmed, "/")[0]
	switch r.Method {
	case http.MethodGet:
		srv, err := h.svc.Get(r.Context(), principal, uuid)
		if err != nil {
			writeDomainErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, srv)
	case http.MethodPatch, http.MethodPut:
		var input UpdateServerInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			writeJSONError(w, http.StatusUnprocessableEntity, "invalid request body")
			return
		}
		srv, err := h.svc.Update(r.Context(), principal, uuid, input)
		if err != nil {
			writeDomainErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, srv)
	case http.MethodDelete:
		if err := h.svc.Delete(r.Context(), principal, uuid); err != nil {
			writeDomainErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Server deleted."})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func writeDomainErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, access.ErrForbidden):
		writeJSONError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrNotFound):
		writeJSONError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrPrivateKeyTeam), errors.Is(err, ErrTransferredServer), errors.Is(err, ErrInvalidLocalhost):
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		writeJSONError(w, http.StatusInternalServerError, err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}
