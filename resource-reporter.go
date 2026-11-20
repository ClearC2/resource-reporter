package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
)

type Alert struct {
	Status string `json:"status"`
	Labels struct {
		Alertname string `json:"alertname"`
		Instance  string `json:"instance"`
	} `json:"labels"`
	Annotations struct {
		Description string `json:"description"`
	} `json:"annotations"`
	StartsAt string `json:"startsAt"`
}

type WebhookPayload struct {
	Status      string  `json:"status"`
	Alerts      []Alert `json:"alerts"`
	ExternalURL string  `json:"externalURL"`
}

type CommandSectionsJSON struct {
	Commands []*CommandSection `json:"commands"`
}

type SlackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type SlackBlock struct {
	Type string    `json:"type"`
	Text SlackText `json:"text"`
}

type SlackBlocks struct {
	Blocks []SlackBlock `json:"blocks"`
}

type ErrorPayload struct {
	Error string `json:"error"`
}

func getAlertHost(alert Alert) string {
	config, _ := getConfig()
	// the config can contain a "hosts" object to map friendly server names to resolvable hostnames
	configHost := config.Hosts[alert.Labels.Instance]
	if configHost != "" {
		return configHost
	}
	// otherwise get the host form parsing the "instance" value from the alert
	return strings.Split(alert.Labels.Instance, ":")[0]
}

func processAlert(alert Alert) {
	config, _ := getConfig()
	host := getAlertHost(alert)
	if host == "" {
		fmt.Printf("Irrelevant alert: %s %s", host, alert.Labels.Alertname)
		return
	}
	params := url.Values{}
	params.Add("token", config.Token)
	params.Add("alertName", alert.Labels.Alertname)
	webhookUrl := fmt.Sprintf("http://%s:5050/report?%s", host, params.Encode())
	res, err := http.Get(webhookUrl)
	if err != nil {
		fmt.Printf("Could not contact: %s\n", webhookUrl)
		return
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	var payload CommandSectionsJSON
	unmarshallErr := json.Unmarshal(body, &payload)
	if unmarshallErr != nil {
		fmt.Println("Could not parse json")
		return
	}
	blocks := []SlackBlock{{
		Type: "header",
		Text: SlackText{
			Type: "plain_text",
			Text: alert.Annotations.Description,
		},
	}}
	for _, command := range payload.Commands {
		blocks = append(blocks, SlackBlock{
			Type: "section",
			Text: SlackText{
				Type: "mrkdwn",
				Text: fmt.Sprintf("*%s*", command.Title),
			},
		})
		blocks = append(blocks, SlackBlock{
			Type: "section",
			Text: SlackText{
				Type: "mrkdwn",
				Text: fmt.Sprintf("```%s```", command.Output),
			},
		})
	}
	jsonStr, _ := json.Marshal(SlackBlocks{Blocks: blocks})
	_, sendErr := http.Post(
		config.Slack,
		"application/json",
		bytes.NewBuffer(jsonStr),
	)
	if sendErr != nil {
		fmt.Println("Could not send to slack")
	}
}

func validateToken(w http.ResponseWriter, r *http.Request) bool {
	token := r.URL.Query().Get("token")
	config, _ := getConfig()
	if token != config.Token {
		writeError(w, "Invalid token")
		return false
	}
	return true
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

	http.HandleFunc("/report", reportHandler(store))

	http.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		if !validateToken(w, r) {
			return
		}
		defer r.Body.Close()
		var payload WebhookPayload
		body, _ := io.ReadAll(r.Body)
		err := json.Unmarshal(body, &payload)
		if err != nil {
			writeError(w, "Could now parse payload")
			return
		}
		var wg sync.WaitGroup
		defer wg.Wait()
		for _, alert := range payload.Alerts {
			if alert.Status == "firing" {
				wg.Add(1)
				go func() {
					processAlert(alert)
					wg.Done()
				}()
			}
		}
	})

	http.ListenAndServe(":5050", nil)
}
