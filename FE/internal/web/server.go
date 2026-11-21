package web

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
)

//go:embed templates static
var assets embed.FS

func NewHandler(backendURL *url.URL) (http.Handler, error) {
	if backendURL == nil {
		return nil, fmt.Errorf("backend URL is required")
	}

	loginTemplate, err := parsePageTemplate("login")
	if err != nil {
		return nil, fmt.Errorf("parse login template: %w", err)
	}

	dashboardTemplate, err := parsePageTemplate("dashboard")
	if err != nil {
		return nil, fmt.Errorf("parse dashboard template: %w", err)
	}

	usersTemplate, err := parsePageTemplate("users")
	if err != nil {
		return nil, fmt.Errorf("parse users template: %w", err)
	}

	groupsTemplate, err := parsePageTemplate("groups")
	if err != nil {
		return nil, fmt.Errorf("parse groups template: %w", err)
	}

	connectionsTemplate, err := parsePageTemplate("connections")
	if err != nil {
		return nil, fmt.Errorf("parse connections template: %w", err)
	}

	grantsTemplate, err := parsePageTemplate("grants")
	if err != nil {
		return nil, fmt.Errorf("parse grants template: %w", err)
	}

	draftsTemplate, err := parsePageTemplate("drafts")
	if err != nil {
		return nil, fmt.Errorf("parse drafts template: %w", err)
	}

	historyTemplate, err := parsePageTemplate("history")
	if err != nil {
		return nil, fmt.Errorf("parse history template: %w", err)
	}

	viewerTemplate, err := parsePageTemplate("viewer")
	if err != nil {
		return nil, fmt.Errorf("parse viewer template: %w", err)
	}

	reviewsTemplate, err := parsePageTemplate("reviews")
	if err != nil {
		return nil, fmt.Errorf("parse reviews template: %w", err)
	}
	jobsTemplate, err := parsePageTemplate("jobs")
	if err != nil {
		return nil, fmt.Errorf("parse jobs template: %w", err)
	}

	staticFiles, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("load static files: %w", err)
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(backendURL)
			r.Out.Host = r.In.Host
			r.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "Backend API tidak tersedia", http.StatusBadGateway)
		},
	}

	mux := http.NewServeMux()

	mux.Handle("/api/", proxy)
	mux.Handle("/health/", proxy)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFiles))))

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	})

	mux.HandleFunc("GET /login", pageHandler(loginTemplate, PageData{
		Title: "Login - Terserah",
	}))

	mux.HandleFunc("GET /dashboard", pageHandler(dashboardTemplate, PageData{
		Title: "Dashboard - Terserah",
	}))

	mux.HandleFunc("GET /reviews", pageHandler(reviewsTemplate, PageData{
		Title: "Migration Reviews - Terserah",
	}))

	mux.HandleFunc("GET /admin/users", pageHandler(usersTemplate, PageData{
		Title: "User Management - Terserah",
	}))

	mux.HandleFunc("GET /admin/groups", pageHandler(groupsTemplate, PageData{
		Title: "Group Management - Terserah",
	}))

	mux.HandleFunc("GET /admin/databases", pageHandler(connectionsTemplate, PageData{
		Title: "Database Connections - Terserah",
	}))

	mux.HandleFunc("GET /admin/grants", pageHandler(grantsTemplate, PageData{
		Title: "Database Grants - Terserah",
	}))

	jobsPage := func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id < 1 {
			http.Error(w, "ID database tidak valid", http.StatusBadRequest)
			return
		}
		jobID := ""
		if value := r.PathValue("jobID"); value != "" {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed < 1 {
				http.Error(w, "ID job tidak valid", http.StatusBadRequest)
				return
			}
			jobID = strconv.FormatInt(parsed, 10)
		}
		pageHandler(jobsTemplate, PageData{Title: "Migration Jobs - Terserah", DatabaseID: strconv.FormatInt(id, 10), JobID: jobID})(w, r)
	}
	mux.HandleFunc("GET /databases/{id}/jobs", jobsPage)
	mux.HandleFunc("GET /databases/{id}/jobs/{jobID}", jobsPage)

	mux.HandleFunc("GET /databases/{id}/viewer", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "ID database tidak valid", http.StatusBadRequest)
			return
		}
		pageHandler(viewerTemplate, PageData{
			Title:      "Database Viewer - Terserah",
			DatabaseID: strconv.FormatInt(id, 10),
		})(w, r)
	})

	mux.HandleFunc("GET /databases/{id}/drafts", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "ID database tidak valid", http.StatusBadRequest)
			return
		}
		pageHandler(draftsTemplate, PageData{
			Title:      "Migration Drafts - Terserah",
			DatabaseID: strconv.FormatInt(id, 10),
		})(w, r)
	})

	mux.HandleFunc("GET /databases/{id}/history", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "ID database tidak valid", http.StatusBadRequest)
			return
		}
		pageHandler(historyTemplate, PageData{
			Title:      "Migration History - Terserah",
			DatabaseID: strconv.FormatInt(id, 10),
		})(w, r)
	})

	mux.HandleFunc("GET /databases/{id}/reviews", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "ID database tidak valid", http.StatusBadRequest)
			return
		}
		pageHandler(reviewsTemplate, PageData{
			Title:      "Migration Reviews - Terserah",
			DatabaseID: strconv.FormatInt(id, 10),
		})(w, r)
	})

	mux.HandleFunc("GET /databases/{id}/reviews/{reviewID}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "ID database tidak valid", http.StatusBadRequest)
			return
		}
		revID, err := strconv.ParseInt(r.PathValue("reviewID"), 10, 64)
		if err != nil || revID <= 0 {
			http.Error(w, "ID review tidak valid", http.StatusBadRequest)
			return
		}
		pageHandler(reviewsTemplate, PageData{
			Title:      "Migration Reviews - Terserah",
			DatabaseID: strconv.FormatInt(id, 10),
			ReviewID:   strconv.FormatInt(revID, 10),
		})(w, r)
	})

	return mux, nil
}
