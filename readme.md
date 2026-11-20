# resource-reporter

An http applicaton that serves as a webhook for AlertManager to send server command outputs to slack.

<img width="1519" alt="image" src="https://user-images.githubusercontent.com/881986/155259015-81a90f99-6151-40d0-a006-ca5f313d3877.png">

This is meant to be installed on all servers being alerted on in prometheus/alertmanager.

## How it works
The application needs a config file to run. The config file specifies alert names and corresponding shell commands relevant to inspecting why that alert was triggered. Here is an example configuration file:

```json
{
    "slack": "https://hooks.slack.com/services/:token1/:token2",
    "token": "secret-token123",
    "alerts": {
        "RAM Used": [
           {"title": "Hostname", "command": "hostname"},
           {"title": "Memory Summary", "command": "free -h"},
           {"title": "ps - Ordered by MEM", "command": "ps ax --sort=-%mem -o pid,ruser=USER,%cpu=%CPU,%mem=%MEM,command | head -n 16"}
        ],
        "CPU Used": [
            {"title": "Hostname", "command": "hostname"},
            {"title": "ps - Ordered by CPU", "command": "ps ax --sort=-%cpu -o pid,ruser=USER,%cpu=%CPU,%mem=%MEM,command | head -n 16"}
        ],
        "Disk Space": [
            {"title": "Hostname", "command": "hostname"},
            {"title": "Disk Space", "command": "df -h"}
        ]
    }
}
```


### Optional settings
Every key below is optional. A zero or missing value means "use the default".

| Key | Default | Meaning |
|---|---|---|
| `repeat_hours` | 6 | The same alert (host, alert name, start time) is reported at most once per this many hours, however often AlertManager repeats it. `0` reports every notification. |
| `command_timeout_seconds` | 20 | Longest a single command may run. A command entry can carry its own `"timeout"` (seconds). A command that overruns has its whole process group killed; what it printed is kept and the output ends with `(timed out after Ns)`. |
| `report_budget_seconds` | 75 | Total time for all of an alert's commands. Commands run one after another inside this budget; commands that no longer fit are skipped and say so. |
| `host_timeout_seconds` | 90 | How long the monitor copy waits for a host's report. |
| `slack_timeout_seconds` | 10 | Timeout for one request to Slack. A 429 or 5xx answer is retried once, waiting for `Retry-After` (at most 30 seconds). |
| `max_output_chars` | 2900 | Command output is cut to this many characters (the `/report` reply and the Slack message), with a "truncated" line saying how much was dropped. |
| `unreachable_notice` | true | `false` turns the "No report could be collected" post off; the failure is only logged, and the next notification tries the host again. |

The app exposes a `/report?alertName=<alertName>&token=<token>` endpoint. Based on the `alertName` query parameter, the endpoint will run every command specified in the config file and respond with the output of each. These `alertName`s correspond with alerts defined in prometheus.

```sh
curl "http://localhost:5050/report?alertName=Disk%20Space&token=secret-token123"
```
```json
{
  "commands": [
    {
      "title": "Hostname",
      "command": "hostname",
      "output": "dadams-mbpro.attlocal.net"
    },
    {
      "title": "Disk Space",
      "command": "df -h",
      "output": "Filesystem       Size   Used  Avail Capacity iused      ifree %iused  Mounted on\n/dev/disk1s5s1  932Gi   22Gi  405Gi     6%  577263 4249848280    0%   /\ndevfs           194Ki  194Ki    0Bi   100%     672          0  100%   /dev\n/dev/disk1s4    932Gi  6.0Gi  405Gi     2%       6 4249848280    0%   /System/Volumes/VM\n/dev/disk1s2    932Gi  509Mi  405Gi     1%    3035 4249848280    0%   /System/Volumes/Preboot\n/dev/disk1s6    932Gi  106Mi  405Gi     1%     462 4249848280    0%   /System/Volumes/Update\n/dev/disk1s1    932Gi  496Gi  405Gi    56% 6845060 4249848280    0%   /System/Volumes/Data\nmap auto_home     0Bi    0Bi    0Bi   100%       0          0  100%   /System/Volumes/Data/home\n/dev/disk1s5    932Gi   22Gi  405Gi     6%  577696 4249848280    0%   /System/Volumes/Update/mnt1"
    }
  ]
}

```

The app also has a `/webhook` endpoint that AlertManager can be configured to use. When an alert is triggered, AlertManager will send a POST request to the resource-reporter app's `/webhook` endpoint. This POST request will contain an array of alerts. Each alert has the server hostname that the alert was triggered for and an alert name("CPU Used", "RAM Used", "Disk Space"). For each alert, the webhook will send a request to the target server's resource-reporter app's `/report?alertName=<alertName>` endpont. The webhook will take that response and send a request to Slack to display a custom message in a designated slack channel with the command outputs(based on the preconfigured slack webhook).

The webhook answers AlertManager at once and does its reporting in the background, one report per firing alert. If the Slack post fails, the alert is not remembered as reported, so AlertManager's next notification tries again. When a host cannot be reached (or answers with something that is not a report), the Slack message carries the alert title and a "No report could be collected" line naming the reason.

Old (2022) and new copies work together in both directions: the `/report` reply is still `{"commands":[{"title","command","output"}]}`, so a new monitor can fetch reports from old hosts and an old monitor can fetch them from new ones. A 2022 settings file runs unchanged.

The `token` in the config file is an authentication token. It needs to be the same across all server installs. AlertManager is configured to use it when making a request to the webhook with the alerts payload. The webhook also uses it in requests to other resource-reporters `/report` endpoint to authenticate.

## Installation
There are two ways to install the resource-reporter.

### Ansible installation
The `ansible/` folder is the 2022 method, used for the CentOS 7 hosts. Rocky 9 hosts are installed by ansibull instead; this folder is left as it was.

Clone the repository. Copy the `config.linux.json` to `ansible/config.deploy.json` and fill out the slack endpoint and token.

Next, create a `ansible/hosts` file that contains a line for every server you would like to deploy to with your ssh username:
```
[servers]
192.168.100.52 ansible_user=johndoe
192.168.100.53 ansible_user=johndoe
192.168.100.54 ansible_user=johndoe
```

Finally deploy to all servers:
```bash
cd ansible
ansible-playbook -i ./hosts playbook.yml
```

### Manual installation
Copy the`config.linux.json` file to `config.deploy.json` and fill in the slack webhook and app token. 

```bash
# download the prebuilt executable
wget https://github.com/ClearC2/resource-reporter/releases/download/<release-tag>/resource-reporter.linux-amd64 

# or clone the repo and build yourself (needs Docker; runs Go 1.27 in a container)
make build   # writes resource-reporter.linux-amd64 and its .sha256
make test    # go test -race ./...
make vet

# deploy
scp ./resource-reporter.linux-amd64 user@server:/home/prometheus/
scp ./config.deploy.json user@server:/home/prometheus/resource-reporter.config.json
```

Create a service file on the target server to run the application:

```service
# /etc/systemd/system/resource-reporter.service
[Unit]
Description=Go resource reporter for AlertManager + Slack
After=network-online.target
[Service]
User=prometheus
Restart=on-failure
ExecStart=/home/prometheus/resource-reporter.linux-amd64 /home/prometheus/resource-reporter.config.json
[Install]
WantedBy=multi-user.target
```
Enable and start the service:
```bash
systemctl enable resource-reporter.service
service resource-reporter start
```

The resource-reporter will be running on port 5050. `resource-reporter --version` prints the version (2.0.0); started with no settings file it prints usage and exits 2, and an unreadable or invalid settings file at start is reported on stderr and exits 1. While running, an edited settings file is picked up without a restart; if the new content is invalid, the last good settings stay in force and an error is logged.

After installing on all target servers we need to configure AlertManager to use it as a webhook.

```yaml
route:
  # default route config...
  routes:
    # other routes...
    - receiver: resource-reporter
      match:
      repeat_interval: 30m
      continue: true
receivers:
  # other receivers...
  - name: resource-reporter
    webhook_configs:
    - url: http://localhost:5050/webhook?token=secrettoken123
```
Restart AlertManager:
```bash
service alertmanager restart
```

#### Running locally
Create a local config file first.
```bash
# run locally
go run . ./config.local.json
```
