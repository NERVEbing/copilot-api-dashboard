package httpserver

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/NERVEbing/copilot-api-dashboard/internal/dashboard"
	"github.com/NERVEbing/copilot-api-dashboard/internal/discovery"
	"github.com/NERVEbing/copilot-api-dashboard/internal/upstream"
	"github.com/NERVEbing/copilot-api-dashboard/web"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func invalid(w http.ResponseWriter, message string) {
	writeJSON(w, 400, struct {
		Data   any                 `json:"data"`
		Errors []discovery.Failure `json:"errors"`
	}{Errors: []discovery.Failure{{Target: "request", Operation: "validation", Message: message}}})
}

func validAccount(login string) bool {
	return login != "" && strings.TrimSpace(login) == login && !strings.ContainsAny(login, "\r\n\t") && len(login) <= 256
}

func params(r *http.Request, events bool) (string, string, int, int, string) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", "", 0, 0, "invalid query string"
	}
	for key, values := range q {
		if len(values) != 1 {
			return "", "", 0, 0, "query parameters must occur once"
		}
		pagination := events && (key == "page" || key == "page_size")
		if key != "period" && key != "account" && !pagination {
			return "", "", 0, 0, "unknown query parameter"
		}
	}
	period := q.Get("period")
	if !q.Has("period") {
		period = "last_30_days"
	}
	if !upstream.ValidPeriod(period) {
		return "", "", 0, 0, "invalid period"
	}
	login := q.Get("account")
	if (q.Has("account") || events) && !validAccount(login) {
		return "", "", 0, 0, "invalid account"
	}
	page, size := 1, 20
	for key, dst := range map[string]*int{"page": &page, "page_size": &size} {
		if !q.Has(key) {
			continue
		}
		value := q.Get(key)
		if value == "" || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return "", "", 0, 0, "invalid pagination"
		}
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || (key == "page_size" && n > 100) {
			return "", "", 0, 0, "invalid pagination"
		}
		*dst = n
	}
	return period, login, page, size, ""
}

func syncAccount(r *http.Request) (string, string) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", "invalid query string"
	}
	for key, values := range q {
		if key != "account" {
			return "", "unknown query parameter"
		}
		if len(values) != 1 {
			return "", "query parameters must occur once"
		}
	}
	login := q.Get("account")
	if q.Has("account") && !validAccount(login) {
		return "", "invalid account"
	}
	return login, ""
}

func New(service *dashboard.Service, basePath string) http.Handler {
	assets := map[string]struct {
		body        []byte
		contentType string
	}{}
	for path, contentType := range map[string]string{"/": "text/html; charset=utf-8", "/app.js": "text/javascript; charset=utf-8", "/styles.css": "text/css; charset=utf-8"} {
		name := strings.TrimPrefix(path, "/")
		if name == "" {
			name = "index.html"
		}
		body, err := fs.ReadFile(web.Files, name)
		if err != nil {
			panic("missing embedded asset: " + name)
		}
		assets[path] = struct {
			body        []byte
			contentType string
		}{body, contentType}
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		requestPath := r.URL.Path
		if basePath != "/" {
			if requestPath == basePath {
				target := basePath + "/"
				if r.URL.RawQuery != "" {
					target += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, target, http.StatusPermanentRedirect)
				return
			}
			if !strings.HasPrefix(requestPath, basePath+"/") {
				http.NotFound(w, r)
				return
			}
			requestPath = strings.TrimPrefix(requestPath, basePath)
		}
		_, asset := assets[requestPath]
		known := asset || requestPath == "/healthz" || requestPath == "/api/v1/dashboard" || requestPath == "/api/v1/events" || requestPath == "/api/v1/sync"
		if !known {
			http.NotFound(w, r)
			return
		}
		if requestPath == "/api/v1/sync" {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			login, err := syncAccount(r)
			if err != "" {
				invalid(w, err)
				return
			}
			failures, found := service.Sync(r.Context(), login)
			status := http.StatusOK
			if !found {
				status = http.StatusNotFound
			}
			writeJSON(w, status, struct {
				Data   map[string]bool     `json:"data"`
				Errors []discovery.Failure `json:"errors"`
			}{Data: map[string]bool{"synced": len(failures) == 0}, Errors: failures})
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if a, ok := assets[requestPath]; ok {
			w.Header().Set("Content-Type", a.contentType)
			_, _ = w.Write(a.body)
			return
		}
		if requestPath == "/healthz" {
			writeJSON(w, 200, map[string]bool{"ok": true})
			return
		}
		period, login, page, size, err := params(r, requestPath == "/api/v1/events")
		if err != "" {
			invalid(w, err)
			return
		}
		if requestPath == "/api/v1/events" {
			result, status := service.Events(r.Context(), login, period, page, size)
			writeJSON(w, status, result)
			return
		}
		result, found := service.Dashboard(r.Context(), period, login)
		status := 200
		if !found {
			status = 404
		}
		writeJSON(w, status, result)
	})
	return http.NewCrossOriginProtection().Handler(handler)
}
