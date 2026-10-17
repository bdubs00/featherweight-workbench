# syntax=docker/dockerfile:1
# fw-workbench:static. A static binary on a distroless base. The build reads
# the kernel from the build context named kernel, because go.mod replaces
# the kernel module with the checkout next to this one. Build with:
#   docker buildx build --build-context kernel=../featherweight-kernel \
#     -t ghcr.io/bdubs00/featherweight-workbench:static .
ARG GO_VERSION=1.27
FROM golang:${GO_VERSION} AS build
WORKDIR /src/featherweight-workbench
COPY --from=kernel go.mod go.sum /src/featherweight-kernel/
COPY go.mod go.sum ./
RUN go mod download
COPY --from=kernel . /src/featherweight-kernel
COPY . .
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
