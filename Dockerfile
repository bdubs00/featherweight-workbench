# syntax=docker/dockerfile:1
# fw-workbench:static. A static binary on a distroless base. The build reads
# the kernel from the build context named kernel, because go.mod replaces
# the kernel module with the checkout next to this one. With the build
# context page, a checkout of featherweight-bossmode, and PAGE=build, a node
# stage builds the page of bossmode and the binary embeds it. Build with
# the page:
#   docker buildx build --build-context kernel=../featherweight-kernel \
#     --build-context page=../featherweight-bossmode --build-arg PAGE=build \
#     -t ghcr.io/bdubs00/featherweight-workbench:static .
# Without the page, the binary embeds the placeholder of the repository:
#   docker buildx build --build-context kernel=../featherweight-kernel \
#     -t ghcr.io/bdubs00/featherweight-workbench:static .
ARG GO_VERSION=1.27
ARG NODE_VERSION=22
# PAGE is build to build the page from the build context page, or none to
# embed the placeholder. BuildKit builds only the stage that PAGE names, so
# a build without the page needs no build context page.
ARG PAGE=none

FROM node:${NODE_VERSION}-bookworm-slim AS page-build
WORKDIR /page
COPY --from=page package.json package-lock.json ./
RUN npm ci
COPY --from=page index.html tsconfig.json vite.config.ts ./
COPY --from=page src ./src
COPY --from=page public ./public
RUN npm run build && mkdir -p /out && cp -R dist/. /out/

FROM golang:${GO_VERSION} AS page-none
RUN mkdir -p /out

FROM page-${PAGE} AS page-out

FROM golang:${GO_VERSION} AS build
WORKDIR /src/featherweight-workbench
COPY --from=kernel go.mod go.sum /src/featherweight-kernel/
COPY go.mod go.sum ./
RUN go mod download
COPY --from=kernel . /src/featherweight-kernel
COPY . .
COPY --from=page-out /out/ bossmode/static/
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/usr/local/bin/fw-workbench ./cmd/fw-workbench \
 && ln -s fw-workbench /out/usr/local/bin/ax-task-runner \
 && ln -s fw-workbench /out/usr/local/bin/fw

FROM gcr.io/distroless/static-debian12:latest
COPY --from=build /out/ /
# ax probes port 80; plain deployments use 8080.
EXPOSE 80 8080
ENTRYPOINT ["/usr/local/bin/fw-workbench"]
CMD ["serve"]
