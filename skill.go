package main

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/golang/glog"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SkillHandler serves the SKILL.md file for a device identified by PSN.
// URL pattern: /{psn}/SKILL.md (no auth required).
// The device model is resolved from the database and used to select the
// appropriate embedded skill markdown file.
func SkillHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse /{psn}/SKILL.md
		path := strings.TrimPrefix(r.URL.Path, "/")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) != 2 || parts[1] != "SKILL.md" || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		psn := parts[0]

		var model *string
		err := pool.QueryRow(r.Context(),
			`SELECT device_model FROM device_information WHERE psn = $1`,
			psn,
		).Scan(&model)
		if err != nil {
			glog.Warningf("skill handler: model lookup failed: psn=%s err=%v", psn, err)
			http.Error(w, "device not found", http.StatusNotFound)
			return
		}
		if model == nil || *model == "" {
			glog.Warningf("skill handler: no model set for psn=%s", psn)
			http.Error(w, "device model not configured", http.StatusNotFound)
			return
		}

		// Skill files are named by uppercase model: PRO.md, ULTRA.md, etc.
		skillPath := fmt.Sprintf("data/skills/%s.md", strings.ToUpper(*model))
		data, err := skillsFS.ReadFile(skillPath)
		if err != nil {
			glog.Warningf("skill handler: no skill file for model=%s psn=%s", *model, psn)
			http.Error(w, fmt.Sprintf("no skill definition for model: %s", *model), http.StatusNotFound)
			return
		}

		glog.V(1).Infof("skill handler: serving %s for psn=%s model=%s", skillPath, psn, *model)
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write(data)
	}
}
