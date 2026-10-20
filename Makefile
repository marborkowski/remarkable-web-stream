GO      ?= go
DIST    := dist
GMS_VERSION ?= v1.4.0

.PHONY: help test build gms deploy fmt clean

help: ## Show this help
	@grep -E '^[a-z]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*## "}{printf "  %-8s %s\n", $$1, $$2}'

test: ## go vet + unit tests (proxy and pentool)
	cd proxy && $(GO) vet ./... && $(GO) test ./...
	cd tools/pentool && GOOS=linux GOARCH=arm64 $(GO) vet .
	for f in deploy.sh gms/build-gms.sh device/*.sh; do sh -n "$$f" || exit 1; done

build: ## Cross-compile the proxy for arm and arm64 into dist/
	mkdir -p $(DIST)
	cd proxy && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags="-s -w" -o ../$(DIST)/rmshare-arm64 .
	cd proxy && CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 $(GO) build -trimpath -ldflags="-s -w" -o ../$(DIST)/rmshare-arm .
	cd tools/pentool && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags="-s -w" -o ../../$(DIST)/pentool-arm64 .

gms: ## Build patched goMarkableStream for arm64 into dist/
	GO=$(GO) GMS_VERSION=$(GMS_VERSION) gms/build-gms.sh arm64 $(DIST)/gomarkablestream-arm64-$(GMS_VERSION)-patched

deploy: ## Build and deploy to the tablet (HOST=root@10.11.99.1)
	./deploy.sh $(HOST)

fmt: ## Format Go code
	cd proxy && $(GO) fmt ./...
	cd tools/pentool && $(GO) fmt ./...

clean: ## Remove build output and the download cache
	rm -rf $(DIST)
