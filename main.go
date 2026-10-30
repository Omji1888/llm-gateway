package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
)

const model = "gemini-3.8-flash"

type chatReq struct {
	Prompt string `json:"prompt"`
}

func chatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req chatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Prompt == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	body, _ := json.Marshal(map[string]any{
		"contents": []any{
			map[string]any{"parts": []any{map[string]any{"text": req.Prompt}}},
		},
	})

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent", model)
	gr, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	gr.Header.Set("Content-Type", "application/json")
	gr.Header.Set("x-goog-api-key", os.Getenv("GEMINI_API_KEY"))

	resp, err := http.DefaultClient.Do(gr)
	if err != nil {
		log.Println("upstream error:", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func main() {
	http.HandleFunc("/chat", chatHandler)
	log.Println("listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
