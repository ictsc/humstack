GO=go
GOOPT=-ldflags="-s -w" -trimpath

.PHONY: all
all: apiserver agent humcli

apiserver:
	$(GO) build $(GOOPT) -o bin/apiserver cmd/apiserver/main.go

agent:
	$(GO) build $(GOOPT) -o bin/agent cmd/agent/main.go

humcli:
	$(GO) build $(GOOPT) -o bin/humcli cmd/humcli/main.go

install:
	install --compare ./bin/apiserver /usr/bin/humstack-apiserver
	install --compare ./bin/agent /usr/bin/humstack-agent
	install --compare ./bin/humcli /usr/bin/humcli
	install --compare ./setup/systemd/humstack-api.service /etc/systemd/system/humstack-api.service
	install --compare ./setup/systemd/humstack-agent.service /etc/systemd/system/humstack-agent.service

run-apiserver:
	$(GO) run cmd/apiserver/main.go --listen-address 0.0.0.0

run-agent:
	sudo $(GO) run cmd/agent/main.go --config cmd/agent/config.yaml
