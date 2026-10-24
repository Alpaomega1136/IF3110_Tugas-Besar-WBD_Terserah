package httpx

import (
	"encoding/json"
	"io"
	"net/http"
)

func JSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func Error(w http.ResponseWriter, status int, code, message string) {
	JSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func Decode(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		Error(w, 400, "invalid_input", "Invalid JSON request")
		return false
	}
	if d.Decode(new(any)) != io.EOF {
		Error(w, 400, "invalid_input", "Expected one JSON object")
		return false
	}
	return true
}
