package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3330"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(
				w,
				"data: {\"model\":\"e2e-response-model\",\"choices\":[{\"delta\":{\"content\":\"streamed \"}}]}\n\n",
			)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			_, _ = fmt.Fprint(
				w,
				"data: {\"choices\":[{\"delta\":{\"content\":\"completion\"}}],\"usage\":{\"prompt_tokens\":23,\"completion_tokens\":4}}\n\ndata: [DONE]\n\n",
			)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(
			w,
			`{"model":"e2e-response-model","choices":[{"message":{"content":"fixed completion"}}],"usage":{"prompt_tokens":23,"completion_tokens":4}}`,
		)
	})
	log.Fatal(http.ListenAndServe("127.0.0.1:"+port, mux))
}
