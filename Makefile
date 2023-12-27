GO=go

.PHONY: all
all: apiserver agent humcli

apiserver:
	$(GO) build -o bin/apiserver cmd/apiserver/main.go

agent:
	$(GO) build -o bin/agent cmd/agent/main.go

humcli:
	$(GO) build -o bin/humcli cmd/humcli/main.go

install:
	install ./bin/apiserver /usr/bin/humstack-apiserver
	install ./bin/agent /usr/bin/humstack-agent
	install ./bin/humcli /usr/bin/humcli
	install ./setup/systemd/humstack-api.service /etc/systemd/system/humstack-api.service
	install ./setup/systemd/humstack-agent.service /etc/systemd/system/humstack-agent.service

run-apiserver:
	$(GO) run cmd/apiserver/main.go --listen-address 0.0.0.0

run-agent:
	sudo $(GO) run cmd/agent/main.go --config cmd/agent/config.yaml
