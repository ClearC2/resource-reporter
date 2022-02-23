# resource-reporter

An http applicaton that serves as a webhook for AlertManager to send server command outputs to slack.

<img width="1519" alt="image" src="https://user-images.githubusercontent.com/881986/155259015-81a90f99-6151-40d0-a006-ca5f313d3877.png">

This is meant to be installed on all servers being alerted on in prometheus/alertmanager.

## How it works
The application needs a config file to run. The config file specifies alert names and corresponding shell commands relevant to inspecting why that alert was triggered. The app exposes a `/report?alertName=<alertName>&token=<token>` endpoint. Based on the `alertName` query parameter, the endpoint will run every command specified in the config file and respond with the output of each. These `alertName`s correspond with alerts defined in prometheus.

When an alert is triggered, AlertManager will send a POST request to the resource-reporter app's `/webhook` endpoint. This POST request will contain an array of alerts. Each alert has the server hostname that the alert was triggered for and an alert name("CPU Used", "RAM Used", "Disk Space"). For each alert, the webhook will send a request to the target server's resource-reporter app's `/report?alertName=<alertName>` endpont. The webhook will take that response and send a request to Slack to display a custom message in a designated slack channel with the command outputs(based on the preconfigured slack webhook).

The `token` in the config file is an authentication token. It needs to be the same across all server installs. AlertManager is configured to use it when making a request to the webhook with the alerts payload. The webhook also uses it in requests to other resource-reporters `/report` endpoint to authenticate.

## Installation
Copy the`config.linux.json` file to `config.deploy.json` and fill in the slack webhook and app token. 

```bash
# download the prebuilt executable
wget https://github.com/ClearC2/resource-reporter/releases/download/v1.0.0/resource-reporter.linux-amd64 

# or clone the repo and build yourself
GOOS=linux GOARCH=amd64 go build -o resource-reporter.linux-amd64 resource-reporter.go

# deploy
scp ./resource-reporter.linux-amd64 user@server:/srv/utils/
scp ./config.deploy.json user@server:/srv/utils/resource-reporter.config.json
```

Create a service file on the target server to run the application:

```service
# /etc/systemd/system/resource-reporter.service
[Unit]
Description=Go resource reporter for AlertManager + Slack
After=network-online.target
[Service]
User=root
Restart=on-failure
ExecStart=/srv/utils/resource-reporter.linux-amd64 /srv/utils/resource-reporter.config.json
[Install]
WantedBy=multi-user.target
```
Enable and start the service:
```bash
systemctl enable resource-reporter.service
service resource-reporter start
```
The resource-reporter will be running on port 5050.

After installing on all target servers we need to configure AlertManager to use it as a webhook.

```yaml
global:
  # other settings
  slack_api_url: <slack-url>

route:
  receiver: slack
  repeat_interval: 1h
  group_by: []
  routes:
    - receiver: slack
      match:
      repeat_interval: 30m
      continue: true
    - receiver: webhook
      match:
      repeat_interval: 30m
      continue: true
receivers:
  - name: webhook
    webhook_configs:
    - url: http://localhost:5050/webhook?token=secrettoken123
  - name: slack
    slack_configs:
    - channel: '#devops-alerts'
      send_resolved: true
```
Restart AlertManager:
```bash
service alertmanager restart
```

#### Running locally
Create a local config file first.
```bash
# run locally
go run resource-reporter.go ./config.local.json
```
