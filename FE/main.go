package main

import (
	"log"
	"net/http"
	"time"

	"terserah/frontend/internal/config"
	"terserah/frontend/internal/web"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	handler, err := web.NewHandler(cfg.BackendURL)
	if err != nil {
		log.Fatal(err)
	}

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("Frontend listening on %s", cfg.HTTPAddr)
	log.Fatal(server.ListenAndServe())
}
