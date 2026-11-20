package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

type ErrorPayload struct {
	Error string `json:"error"`
}

func writeJson(w http.ResponseWriter, payload []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(payload)
}

func writeError(w http.ResponseWriter, err string) {
	payload, _ := json.Marshal(ErrorPayload{Error: err})
	w.Header().Set("Content-Type", "application/json") // must precede WriteHeader to take effect
	w.WriteHeader(http.StatusBadRequest)
	w.Write(payload)
}

// version is set at build time: -ldflags "-X main.version=2.0.0".
var version = "dev"

// newMux routes /report (the host side) and /webhook (the monitor side).
func newMux(store *ConfigStore, r *Reporter) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/report", reportHandler(store))
	mux.HandleFunc("/webhook", r.webhookHandler())
	return mux
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: resource-reporter <settings-file>\n       resource-reporter --version")
		os.Exit(2)
	}
	if os.Args[1] == "--version" {
		fmt.Println("resource-reporter " + version)
		return
	}
	path := os.Args[1]
	store, err := NewConfigStore(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not load settings %s: %v\n", path, err)
		os.Exit(1)
	}
	srv := &http.Server{
		Addr:              ":5050",
		Handler:           newMux(store, NewReporter(store)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("resource-reporter %s listening on %s", version, srv.Addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
