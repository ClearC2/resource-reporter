package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
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

type CommandSection struct {
	Title   string `json:"title"`
	Command string `json:"command"`
	Output  string `json:"output"`
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

type Config struct {
	Slack  string                       `json:"slack"`
	Token  string                       `json:"token"`
	Alerts map[string][]*CommandSection `json:"alerts"`
	Hosts  map[string]string            `json:"hosts"`
}

type ErrorPayload struct {
	Error string `json:"error"`
}

func getConfig() (*Config, error) {
	file, fileErr := os.ReadFile(os.Args[1])
	if fileErr != nil {
		return nil, fileErr
	}
	var config Config
	err := json.Unmarshal(file, &config)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

func getHost(host string) string {
	config, _ := getConfig()
	// try to get host from config
	configHost := config.Hosts[host]
	if configHost != "" {
		return configHost
	}
	// otherwise get the host form parsing the "instance" value from the alert
	return strings.Split(host, ":")[0]
}

func processAlert(wg *sync.WaitGroup, alert Alert) {
	defer wg.Done()
	config, _ := getConfig()
	host := getHost(alert.Labels.Instance)
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
	body, _ := ioutil.ReadAll(res.Body)
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

func runCommandsAndRespond(w http.ResponseWriter, sections []*CommandSection) {
	for _, commandSection := range sections {
		out, err := exec.Command("bash", "-c", commandSection.Command).Output()
		if err != nil {
			commandSection.Output = err.Error()
		} else {
			commandSection.Output = strings.TrimSpace(string(out))
		}
	}

	jsonResponse := CommandSectionsJSON{Commands: sections}
	jsonResp, err := json.Marshal(jsonResponse)
	if err != nil {
		writeError(w, "Could not encode command output")
	} else {
		writeJson(w, jsonResp)
	}
}

func writeJson(w http.ResponseWriter, payload []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(payload)
}

func writeError(w http.ResponseWriter, err string) {
	payload, _ := json.Marshal(ErrorPayload{Error: err})
	w.WriteHeader(http.StatusBadRequest)
	writeJson(w, payload)
}

func main() {
	_, configErr := getConfig()
	if configErr != nil {
		fmt.Println("Could not parse config")
		return
	}

	http.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		if !validateToken(w, r) {
			return
		}
		alertName := r.URL.Query().Get("alertName")
		if alertName == "" {
			writeError(w, "Missing alert name")
			return
		}
		config, _ := getConfig()
		var commands []*CommandSection
		if len(config.Alerts[alertName]) > 0 {
			commands = config.Alerts[alertName]
		} else {
			commands = []*CommandSection{}
		}
		runCommandsAndRespond(w, commands)
	})

	http.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		if !validateToken(w, r) {
			return
		}
		defer r.Body.Close()
		var payload WebhookPayload
		body, _ := ioutil.ReadAll(r.Body)
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
				go processAlert(&wg, alert)
			}
		}
	})

	http.ListenAndServe(":5050", nil)
}
