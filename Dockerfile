# One build, two images:
#   anton          (default target) every Go binary on distroless: operator,
#                  router, mock replica/tier, bench.
#   anton-sandbox  (--target sandbox) the agent sandbox: the agent binary and
#                  OpenCode on Alpine with a shell and git (a real terminal).
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/ ./cmd/...

# OpenCode ships as one native binary; npm installs it (plus every other
# platform build). Install in a throwaway stage and keep only the binary its
# postinstall picked for this platform (musl, since this stage is Alpine).
FROM node:22-alpine AS opencode
ARG OPENCODE_VERSION=1.19.5
RUN npm install -g --no-audit --no-fund "opencode-ai@${OPENCODE_VERSION}" \
 && cp "$(npm root -g)/opencode-ai/bin/opencode.exe" /opencode && /opencode --version

FROM alpine:3.20 AS sandbox
RUN apk add --no-cache bash git python3 libstdc++ libgcc ripgrep \
 && adduser -D -u 1000 agent && mkdir /workspace && chown agent /workspace \
 && printf '%s\n' 'export PS1="agent@sandbox:\\w\\$ "' > /etc/profile.d/anton.sh
COPY --from=opencode /opencode /usr/local/bin/opencode
COPY --from=build /out/sim /bin/sim
USER agent
WORKDIR /workspace
ENTRYPOINT []

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/ /bin/
