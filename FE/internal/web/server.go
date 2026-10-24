package web

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
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

	mux.HandleFunc("GET /login",
		pageHandler(loginTemplate, PageData{
			Title: "Login - Terserah",
		}),
	)

	mux.HandleFunc("GET /dashboard",
		pageHandler(dashboardTemplate, PageData{
			Title: "Dashboard - Terserah",
		}),
	)

	return mux, nil
}
