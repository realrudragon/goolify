package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
)

const maxFileVolumeContentBytes = 5_242_880 // Matches App\Models\LocalFileVolume::MAX_CONTENT_SIZE

var (
	ErrInvalidStoragePath = errors.New("invalid storage path")
	ErrContentTooLarge    = errors.New("file content exceeds 5 MiB limit")
)

// validateStoragePath mirrors LocalFileVolume::resolveHostPath / validateShellSafePath security checks.
func validateStoragePath(p string, allowRelative bool) (string, error) {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" {
		return "", ErrInvalidStoragePath
	}
	if strings.HasPrefix(trimmed, "~") {
		return "", ErrInvalidStoragePath
	}
	for _, bad := range []string{";", "|", "&", "`", "$(", "<", ">", "\n", "\r", "\x00"} {
		if strings.Contains(trimmed, bad) {
			return "", ErrInvalidStoragePath
		}
	}
	for _, seg := range strings.Split(trimmed, "/") {
		if seg == "." || seg == ".." {
			return "", ErrInvalidStoragePath
		}
	}
	if !allowRelative && !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	if trimmed == "/" {
		return "", ErrInvalidStoragePath
	}
	return trimmed, nil
}

func (a *App) handleResourceStorages(
	w http.ResponseWriter,
	r *http.Request,
	p auth.Principal,
	morphType string,
	resType string,
	resID int64,
	resUUID string,
	teamID int64,
	storageUUID string,
) {
	ref := access.ResourceRef{Type: resType, ID: resID, UUID: resUUID, TeamID: &teamID, APISegment: resType + "s"}

	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		canReadSecrets := a.Authz.Check(r.Context(), p, access.ActionReadSensitive, ref) == nil

		persistent := make([]map[string]any, 0)
		pRows, err := a.Pool.Query(r.Context(), `
			SELECT id, uuid, name, mount_path, COALESCE(host_path,''), is_preview_suffix_enabled
			FROM local_persistent_volumes
			WHERE resource_type = $1 AND resource_id = $2
			ORDER BY id ASC
		`, morphType, resID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer pRows.Close()
		for pRows.Next() {
			var id int64
			var u, n, mp, hp string
			var prev bool
			if pRows.Scan(&id, &u, &n, &mp, &hp, &prev) == nil {
				persistent = append(persistent, map[string]any{
					"id": id, "uuid": u, "name": n, "mount_path": mp, "host_path": hp, "is_preview_suffix_enabled": prev,
				})
			}
		}

		files := make([]map[string]any, 0)
		fRows, err := a.Pool.Query(r.Context(), `
			SELECT id, uuid, fs_path, mount_path, content, is_directory, is_host_file
			FROM local_file_volumes
			WHERE resource_type = $1 AND resource_id = $2
			ORDER BY id ASC
		`, morphType, resID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer fRows.Close()
		for fRows.Next() {
			var id int64
			var u, fp, mp string
			var encContent *string
			var isDir, isHost bool
			if fRows.Scan(&id, &u, &fp, &mp, &encContent, &isDir, &isHost) == nil {
				row := map[string]any{
					"id": id, "uuid": u, "fs_path": fp, "mount_path": mp,
					"is_directory": isDir, "is_host_file": isHost,
				}
				if canReadSecrets && encContent != nil && *encContent != "" {
					if a.Cipher != nil {
						if dec, decErr := a.Cipher.DecryptLaravelPayload(*encContent, false); decErr == nil {
							row["content"] = string(dec)
						}
					} else {
						row["content"] = *encContent
					}
				}
				files = append(files, row)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"persistent_storages": persistent,
			"file_storages":       files,
		})

	case http.MethodPost:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Type        string `json:"type"` // persistent | file
			Name        string `json:"name"`
			MountPath   string `json:"mount_path"`
			HostPath    string `json:"host_path"`
			FSPath      string `json:"fs_path"`
			Content     string `json:"content"`
			IsDirectory bool   `json:"is_directory"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid storage payload"})
			return
		}
		mountPath, err := validateStoragePath(body.MountPath, false)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid mount_path"})
			return
		}

		if body.Type == "file" || body.FSPath != "" {
			if len(body.Content) > maxFileVolumeContentBytes {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": ErrContentTooLarge.Error()})
				return
			}
			fsPath, err := validateStoragePath(body.FSPath, true)
			if err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid fs_path"})
				return
			}
			var storedContent *string
			if !body.IsDirectory {
				val := body.Content
				if a.Cipher != nil {
					enc, err := a.Cipher.EncryptLaravelPayload([]byte(val), false)
					if err != nil {
						writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
						return
					}
					val = enc
				}
				storedContent = &val
			}
			u := a.Hooks.NewCUID2()
			var id int64
			err = a.Pool.QueryRow(r.Context(), `
				INSERT INTO local_file_volumes (uuid, fs_path, mount_path, content, is_directory, resource_type, resource_id)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				RETURNING id
			`, u, fsPath, mountPath, storedContent, body.IsDirectory, morphType, resID).Scan(&id)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{
				"id": id, "uuid": u, "type": "file", "fs_path": fsPath, "mount_path": mountPath, "is_directory": body.IsDirectory,
			})
			return
		}

		name := compat.SanitizeString(body.Name)
		if name == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "volume name is required."})
			return
		}
		var hostPath *string
		if strings.TrimSpace(body.HostPath) != "" {
			hp, err := validateStoragePath(body.HostPath, false)
			if err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid host_path"})
				return
			}
			hostPath = &hp
		}
		u := a.Hooks.NewCUID2()
		var id int64
		err = a.Pool.QueryRow(r.Context(), `
			INSERT INTO local_persistent_volumes (uuid, name, mount_path, host_path, resource_type, resource_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id
		`, u, name, mountPath, hostPath, morphType, resID).Scan(&id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": id, "uuid": u, "type": "persistent", "name": name, "mount_path": mountPath,
		})

	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		if storageUUID == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "storage_uuid required"})
			return
		}
		tag1, _ := a.Pool.Exec(r.Context(), `DELETE FROM local_persistent_volumes WHERE uuid = $1 AND resource_type = $2 AND resource_id = $3`, storageUUID, morphType, resID)
		tag2, _ := a.Pool.Exec(r.Context(), `DELETE FROM local_file_volumes WHERE uuid = $1 AND resource_type = $2 AND resource_id = $3`, storageUUID, morphType, resID)
		if tag1.RowsAffected() == 0 && tag2.RowsAffected() == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "storage not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Storage deleted."})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}
