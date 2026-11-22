package controlplane

import (
	"errors"
	"net/http"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/jackc/pgx/v5"
)

func (a *App) handleUIResourcePermissions(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	kind := r.URL.Query().Get("type")
	if kind != "application" && kind != "database" && kind != "service" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Unsupported resource type."})
		return
	}
	resource, err := a.loadResourceMeta(r.Context(), kind, r.URL.Query().Get("uuid"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Resource not found."})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	ref := resource.TeamScopedRef
	ref.ServerID = &resource.ServerID
	if err := a.Authz.Check(r.Context(), p, access.ActionRead, ref); err != nil {
		writeDomainError(w, err)
		return
	}
	permissions := make(map[string]bool)
	for name, action := range map[string]access.Action{"update": access.ActionUpdate, "deploy": access.ActionDeploy, "delete": access.ActionDelete, "read_sensitive": access.ActionReadSensitive, "terminal": access.ActionTerminal} {
		permissions[name] = a.Authz.Check(r.Context(), p, action, ref) == nil
	}
	writeJSON(w, http.StatusOK, permissions)
}
