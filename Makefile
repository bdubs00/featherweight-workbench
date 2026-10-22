GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/bdubs00/featherweight-workbench
KERNEL  ?= ../featherweight-kernel
PAGE    ?= ../featherweight-bossmode
STATIC  := bossmode/static

.PHONY: build test vet fmt run page page-clean image image-toolbox clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/fw-workbench ./cmd/fw-workbench

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

run: build
	./bin/fw-workbench serve

# The page of bossmode: build the checkout of featherweight-bossmode in
# PAGE and copy its dist directory into bossmode/static, which the binary
# embeds. git ignores the copy. page-clean removes it, so the binary
# embeds the placeholder in bossmode/placeholder again.
page:
	cd $(PAGE) && npm ci && npm run build
	test -f $(PAGE)/dist/index.html
	$(MAKE) page-clean
	cp -R $(PAGE)/dist/. $(STATIC)/

page-clean:
	find $(STATIC) -mindepth 1 -maxdepth 1 ! -name .gitkeep -exec rm -rf {} +

# The images read the kernel from KERNEL, as go.mod does, and build the page
# of bossmode from PAGE when PAGE holds a checkout of featherweight-bossmode.
# Without one, the binary embeds the placeholder.
PAGE_CONTEXT = $(if $(wildcard $(PAGE)/package.json),--build-context page=$(PAGE) --build-arg PAGE=build,--build-arg PAGE=none)

image:
	docker buildx build --load --build-context kernel=$(KERNEL) $(PAGE_CONTEXT) --build-arg VERSION=$(VERSION) -t $(IMAGE):static -t $(IMAGE):$(VERSION) .

image-toolbox:
	docker buildx build --load --build-context kernel=$(KERNEL) $(PAGE_CONTEXT) --build-arg VERSION=$(VERSION) -f Dockerfile.toolbox -t $(IMAGE):toolbox .

clean:
	rm -rf bin
