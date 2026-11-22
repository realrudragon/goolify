package controlplane

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
)

func (a *App) handleGitSourceDetails(w http.ResponseWriter, r *http.Request, p auth.Principal, provider, uuid string) {
	table := "gitlab_apps"
	if provider == "gitea" {
		table = "gitea_apps"
	} else if provider != "gitlab" {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	if strings.Contains(uuid, "/") {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	var id, teamID int64
	var name, apiURL, htmlURL, resolvedUUID string
	if err := a.Pool.QueryRow(r.Context(), fmt.Sprintf(`SELECT id,team_id,name,api_url,html_url,uuid FROM %s WHERE uuid=$1 OR id::text=$1`, table), uuid).Scan(&id, &teamID, &name, &apiURL, &htmlURL, &resolvedUUID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Source not found."})
		return
	}
	uuid = resolvedUUID
	ref := access.ResourceRef{Type: provider + "_app", ID: id, UUID: uuid, TeamID: &teamID, APISegment: provider + "-apps"}
	action := access.ActionRead
	if r.Method == http.MethodPatch {
		action = access.ActionUpdate
	}
	if r.Method == http.MethodDelete {
		action = access.ActionDelete
	}
	if err := a.Authz.Check(r.Context(), p, action, ref); err != nil {
		writeDomainError(w, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPatch:
		var body struct {
			Name          *string `json:"name"`
			APIURL        *string `json:"api_url"`
			HTMLURL       *string `json:"html_url"`
			AppSecret     *string `json:"app_secret"`
			ClientSecret  *string `json:"client_secret"`
			AccessToken   *string `json:"access_token"`
			WebhookSecret *string `json:"webhook_secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Invalid source update."})
			return
		}
		for _, value := range []*string{body.APIURL, body.HTMLURL} {
			if value != nil {
				u, err := url.Parse(strings.TrimSpace(*value))
				if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
					writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Source URLs must be absolute HTTP or HTTPS URLs."})
					return
				}
			}
		}
		if body.Name != nil {
			name = compat.SanitizeString(*body.Name)
			if strings.TrimSpace(name) == "" {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Name is required."})
				return
			}
		}
		if body.APIURL != nil {
			apiURL = strings.TrimSpace(*body.APIURL)
		}
		if body.HTMLURL != nil {
			htmlURL = strings.TrimSpace(*body.HTMLURL)
		}
		args := []any{name, apiURL, htmlURL}
		sets := []string{"name=$1", "api_url=$2", "html_url=$3", "updated_at=NOW()"}
		secrets := map[string]*string{"app_secret": body.AppSecret}
		if provider == "gitea" {
			secrets = map[string]*string{"client_secret": body.ClientSecret, "access_token": body.AccessToken, "webhook_secret": body.WebhookSecret}
		}
		for column, value := range secrets {
			if value != nil {
				encrypted, err := a.encryptSecretString(*value)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
					return
				}
				args = append(args, encrypted)
				sets = append(sets, fmt.Sprintf("%s=$%d", column, len(args)))
			}
		}
		args = append(args, id)
		if _, err := a.Pool.Exec(r.Context(), fmt.Sprintf("UPDATE %s SET %s WHERE id=$%d", table, strings.Join(sets, ","), len(args)), args...); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
	case http.MethodDelete:
		result, err := a.Pool.Exec(r.Context(), fmt.Sprintf(`DELETE FROM %s WHERE id=$1 AND NOT EXISTS (SELECT 1 FROM applications WHERE source_id=$1 AND source_type LIKE $2)`, table), id, "%"+map[string]string{"gitlab": "GitlabApp", "gitea": "GiteaApp"}[provider])
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		if result.RowsAffected() == 0 {
			writeJSON(w, http.StatusConflict, map[string]string{"message": "Remove attached applications before deleting this source."})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Source deleted."})
		return
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "uuid": uuid, "name": name, "api_url": apiURL, "html_url": htmlURL, "team_id": teamID})
}
