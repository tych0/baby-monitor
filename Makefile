APP := baby-monitor

.PHONY: run build tidy fmt vet

run:
	go run .

build:
	go build -o $(APP) .

tidy:
	go mod tidy

fmt:
	gofmt -w *.go

vet:
	go vet ./...
