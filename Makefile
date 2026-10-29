BIN := bin
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null)
DATE ?= $(shell date -u +%Y-%m-%d)
PKG := github.com/hbroek/wako/internal/version
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)
CMDS := wakod wako
TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: build test cross release docker fuzz clean
build:
	@for c in $(CMDS); do \
		CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/$$c ./cmd/$$c || exit 1; \
	done

test:
	go vet ./... && go test ./...

cross:
	@for c in $(CMDS); do for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "$$c-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(BIN)/$$c-$$os-$$arch$$ext ./cmd/$$c || exit 1; \
	done; done

# Archives (one per target, both programs inside) and a checksum file, in dist/.
release: cross
	@rm -rf dist && mkdir -p dist
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		d=dist/wako-$(VERSION)-$$os-$$arch; mkdir -p $$d; \
		for c in $(CMDS); do cp $(BIN)/$$c-$$os-$$arch$$ext $$d/$$c$$ext; done; \
		cp README.md $$d/; \
		if [ $$os = windows ]; then (cd dist && zip -qr $$(basename $$d).zip $$(basename $$d)); \
		else tar -C dist -czf $$d.tar.gz $$(basename $$d); fi; \
		rm -rf $$d; \
	done
	@cd dist && (shasum -a 256 *.tar.gz *.zip > SHA256SUMS) && cat SHA256SUMS

docker:
	docker build -t wako:$(VERSION) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) .

# A short fuzzing round of every target (FUZZTIME=2m for a longer one).
FUZZTIME ?= 15s
fuzz:
	@for t in img:FuzzProcess img:FuzzScale diff:FuzzApplyNeverPanics diff:FuzzDiffRoundTrip wiki:FuzzRewriteNeverBreaksText wiki:FuzzLinkResolution \
		store:FuzzPaths store:FuzzSearchText store:FuzzSearchAny archive:FuzzImport mcp:FuzzHandle mcp:FuzzSnippetText ai:FuzzLinkCitations ai:FuzzCleanDraftAndKeywords server:FuzzRequestPaths render:FuzzRenderIsAlwaysSafe render:FuzzRewriteImageRefKeepsTheTextSafe; do \
		pkg=$${t%%:*}; name=$${t#*:}; echo "== $$pkg $$name"; \
		go test ./internal/$$pkg -run '^$$' -fuzz="^$$name\$$" -fuzztime=$(FUZZTIME) || exit 1; \
	done

clean:
	rm -rf $(BIN) dist data
