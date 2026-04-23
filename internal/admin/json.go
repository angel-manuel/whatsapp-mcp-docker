package admin

import (
	"encoding/json"
	"net/http"
)

// errorBody is the shape of all JSON error responses from admin endpoints.
// Clients may rely on the stable `code` field.
type errorBody struct {
	Error   string `json:"error"`
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{
		Error:   http.StatusText(status),
		Code:    code,
		Message: message,
	})
}
