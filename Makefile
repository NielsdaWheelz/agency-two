.PHONY: fmt format vet test test-static test-unit test-storage test-git test-tmux test-runner test-supervisor test-tui test-app test-integration verify verify-full

fmt:
	test -z "$$(gofmt -l cmd internal)"

vet:
	go vet ./...

format:
	gofmt -w cmd internal

test-static: fmt vet

test: test-static
	go test ./...

test-unit:
	go test ./internal/agency/content ./internal/agency/provider ./internal/agency/runnerproto

test-storage:
	go test ./internal/agency/storage

test-git:
	go test ./internal/agency/gitx -count=1

test-tmux:
	go test ./internal/agency/tmux -count=1 -v

test-runner:
	go test ./internal/agency/runnerproto ./internal/agency/runner -count=1

test-supervisor:
	go test ./internal/agency/supervisor -count=1 -v

test-tui:
	go test ./internal/agency/tui -count=1 -v

test-app:
	go test ./internal/agency/app -count=1 -v

test-integration: test-storage test-git test-tmux test-runner test-supervisor test-tui test-app

verify: test-static test-unit test-storage

verify-full: test-static test-unit test-integration test
