package main

import (
    "html/template"
    "log"
    "net/http"
)

func main() {
    tmpl, err := template.ParseFiles("templates/login.html")
    if err != nil {
        log.Fatal(err)
    }

    mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

    mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
        http.Redirect(w, r, "/login", http.StatusSeeOther)
    })

    mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
        data := struct {
            Title string
        }{
            Title: "Login - Terserah",
        }

        err := tmpl.Execute(w, data)
        if err != nil {
            log.Println("Gagal render template:", err)
        }
    })

    log.Println("Frontend berjalan di http://localhost:3000")

    log.Fatal(http.ListenAndServe(":3000", mux))
}