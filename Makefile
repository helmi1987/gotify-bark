BUILDDIR=./build
GOTIFY_VERSION=master
PLUGIN_NAME=bark-forwarder
GO_VERSION=`cat $(BUILDDIR)/gotify-server-go-version`
DOCKER_BUILD_IMAGE=gotify/build
DOCKER_WORKDIR=/proj
DOCKER_RUN=docker run --rm -v "$$PWD/.:${DOCKER_WORKDIR}" -v "`go env GOPATH`/pkg/mod/.:/go/pkg/mod:ro" -w ${DOCKER_WORKDIR}
GO_BUILD_FLAGS=-mod=readonly -a -installsuffix cgo -ldflags "$$LD_FLAGS" -buildmode=plugin
DOCKER_GO_BUILD=go build ${GO_BUILD_FLAGS}

download-tools:
	go install github.com/gotify/plugin-api/cmd/gomod-cap@latest

create-build-dir:
	mkdir -p ${BUILDDIR} || true

# Align go.mod with the go.mod of the targeted Gotify server version (required for plugin loading).
update-go-mod: create-build-dir
	wget -LO ${BUILDDIR}/gotify-server.mod https://raw.githubusercontent.com/gotify/server/${GOTIFY_VERSION}/go.mod
	gomod-cap -from ${BUILDDIR}/gotify-server.mod -to go.mod
	rm ${BUILDDIR}/gotify-server.mod || true
	go mod tidy

get-gotify-server-go-version: create-build-dir
	rm ${BUILDDIR}/gotify-server-go-version || true
	wget -LO ${BUILDDIR}/gotify-server-go-version https://raw.githubusercontent.com/gotify/server/${GOTIFY_VERSION}/GO_VERSION

build-linux-amd64: get-gotify-server-go-version update-go-mod
	${DOCKER_RUN} ${DOCKER_BUILD_IMAGE}:$(GO_VERSION)-linux-amd64 ${DOCKER_GO_BUILD} -o ${BUILDDIR}/${PLUGIN_NAME}-linux-amd64${FILE_SUFFIX}.so ${DOCKER_WORKDIR}

build-linux-arm-7: get-gotify-server-go-version update-go-mod
	${DOCKER_RUN} ${DOCKER_BUILD_IMAGE}:$(GO_VERSION)-linux-arm-7 ${DOCKER_GO_BUILD} -o ${BUILDDIR}/${PLUGIN_NAME}-linux-arm-7${FILE_SUFFIX}.so ${DOCKER_WORKDIR}

build-linux-arm64: get-gotify-server-go-version update-go-mod
	${DOCKER_RUN} ${DOCKER_BUILD_IMAGE}:$(GO_VERSION)-linux-arm64 ${DOCKER_GO_BUILD} -o ${BUILDDIR}/${PLUGIN_NAME}-linux-arm64${FILE_SUFFIX}.so ${DOCKER_WORKDIR}

# Build without Docker on a Linux host (glibc, gcc installed).
# Gotify checks a fingerprint of every shared package, and that fingerprint includes the
# source file paths. The plugin therefore only loads when it is built with the same layout
# as the gotify/build image: Go ${GO_VERSION} installed at /usr/local/go and the module
# cache at /go/pkg/mod. This target enforces both.
build-local: get-gotify-server-go-version update-go-mod
	@test "$$(/usr/local/go/bin/go version 2>/dev/null | awk '{print $$3}')" = "go$(GO_VERSION)" || \
		{ echo "ERROR: /usr/local/go must be Go $(GO_VERSION) (the version Gotify ${GOTIFY_VERSION} was built with)."; exit 1; }
	CGO_ENABLED=1 GOTOOLCHAIN=local GOMODCACHE=/go/pkg/mod PATH=/usr/local/go/bin:$$PATH \
		go build ${GO_BUILD_FLAGS} -o ${BUILDDIR}/${PLUGIN_NAME}-linux-$$(go env GOARCH)${FILE_SUFFIX}.so .

build: build-linux-arm-7 build-linux-amd64 build-linux-arm64

test:
	go test ./...

.PHONY: build build-local test download-tools create-build-dir update-go-mod get-gotify-server-go-version
