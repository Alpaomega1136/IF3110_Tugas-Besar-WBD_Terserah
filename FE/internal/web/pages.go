package web

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
)

type PageData struct {
	Title      string
	DatabaseID string
	Path       string
}

func parsePageTemplate(name string) (*template.Template, error) {
	tmpl, err := template.New(name).ParseFS(assets, "templates/layouts/base.html", fmt.Sprintf("templates/pages/%s.html", name))
	if err != nil {
		return nil, err
	}
	return tmpl, nil
}

func pageHandler(tmpl *template.Template, data PageData) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page := data
		page.Path = r.URL.Path
		var rendered bytes.Buffer
		if err := tmpl.ExecuteTemplate(&rendered, "base", page); err != nil {
			log.Printf("render %q page: %v", data.Title, err)
			http.Error(w, "Gagal memuat halaman", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")

		_, _ = io.Copy(w, &rendered)
	}
}
