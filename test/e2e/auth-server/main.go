package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/config", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":        "e2e-secret-scorer",
			"description": "Secret refresh e2e scorer",
			"source": map[string]any{
				"type": "Prometheus",
				"host": "http://dsf-e2e-auth-server.dynamic-scoring.svc.cluster.local:8080",
				"path": "/api/v1/query_range",
				"params": map[string]any{
					"query": "up",
					"range": 60,
					"step":  15,
				},
			},
			"scoring": map[string]any{
				"host": "http://dsf-e2e-auth-server.dynamic-scoring.svc.cluster.local:8080",
				"path": "/scoring",
				"params": map[string]any{
					"name":     "e2e-secret-score",
					"interval": 2,
				},
			},
		})
	})
	mux.HandleFunc("/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("e2e-auth path=source authorization=%q", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "matrix",
				"result": []any{map[string]any{
					"metric": map[string]string{"node": "e2e-node"},
					"values": [][2]any{{time.Now().Unix(), "1"}},
				}},
			},
		})
	})
	mux.HandleFunc("/scoring", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("e2e-auth path=scoring authorization=%q", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"metric":{"node":"e2e-node"},"score":42}]}`))
	})

	log.Fatal(http.ListenAndServe(":8080", mux))
}
