package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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

func main() {
	store, err := NewConfigStore(os.Args[1])
	if err != nil {
		fmt.Println("Could not parse config:", err)
		return
	}
	reporter := NewReporter(store)

	http.HandleFunc("/report", reportHandler(store))
	http.HandleFunc("/webhook", reporter.webhookHandler())

	http.ListenAndServe(":5050", nil)
}
