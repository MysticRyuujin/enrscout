.PHONY: build crawler api compile test test-race test-nethermind-compat lint staticcheck vulncheck \
	web-install web-audit web-build ci ci-go ci-web tidy run-crawler run-api e2e

NETWORK ?= mainnet
ADVERTISER_NETWORKS ?= mainnet,hoodi,sepolia
GOFLAGS ?=
SOURCE_REVISION ?= unknown
SOURCE_URL ?= https://github.com/MysticRyuujin/enrscout
BUILDINFO_LDFLAGS = -X github.com/MysticRyuujin/enrscout/internal/buildinfo.Revision=$(SOURCE_REVISION) -X github.com/MysticRyuujin/enrscout/internal/buildinfo.SourceURL=$(SOURCE_URL)
NETHERMIND_IMAGES ?= nethermind/nethermind:1.35.2 nethermind/nethermind:1.35.8 nethermind/nethermind:1.36.0 nethermind/nethermind:1.37.2 nethermind/nethermind:1.38.1 nethermind/nethermind:1.39.1
STATICCHECK_VERSION ?= 2026.1
GOVULNCHECK_VERSION ?= v1.6.0

build: crawler api

crawler:
	go build $(GOFLAGS) -ldflags "$(BUILDINFO_LDFLAGS)" -o bin/enrscout-crawler ./cmd/crawler

api:
	go build $(GOFLAGS) -ldflags "$(BUILDINFO_LDFLAGS)" -o bin/enrscout-api ./cmd/api

compile:
	go build -trimpath ./...

test:
	go test ./...

test-race:
	go test -race -shuffle=on ./...

test-nethermind-compat:
	ENRSCOUT_NETHERMIND_IMAGES="$(NETHERMIND_IMAGES)" go test -v ./internal/enrich -run TestNethermindRLPxCompatibility -count=1

lint:
	go vet ./...
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needs to run on:"; echo "$$unformatted"; \
		gofmt -d .; \
		exit 1; \
	fi

staticcheck:
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

web-install:
	cd web && npm ci --ignore-scripts

web-audit:
	cd web && npm audit --audit-level=moderate

web-build:
	cd web && npm run build

# CI invokes these targets so the check list has no second copy to drift from.
ci-go: compile lint staticcheck test-race vulncheck

ci-web: web-install web-audit web-build

ci: ci-go ci-web

tidy:
	go mod tidy

run-crawler:
	go run ./cmd/crawler --advertiser-networks $(ADVERTISER_NETWORKS)

run-api:
	go run ./cmd/api

e2e:
	cd web && node e2e/browse.mjs
