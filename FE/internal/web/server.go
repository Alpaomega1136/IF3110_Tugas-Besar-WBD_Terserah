package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
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

	loginTemplate, err := template.New("login").ParseFS(
		assets,
		"templates/layouts/base.html",
		"templates/pages/login.html",
	)
	if err != nil {
		return nil, fmt.Errorf("parse login template: %w", err)
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
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		var rendered bytes.Buffer
		if err := loginTemplate.ExecuteTemplate(&rendered, "base", PageData{Title: "Login - Terserah"}); err != nil {
			log.Printf("render login template: %v", err)
			http.Error(w, "Gagal memuat halaman", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.Copy(w, &rendered)
	})

	return mux, nil
}

type PageData struct {
	Title string
}
