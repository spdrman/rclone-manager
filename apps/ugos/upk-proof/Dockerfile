# Minimal UPK hardware proof (issue #91). Build from the repo root:
#   docker build -f apps/ugos/upk-proof/Dockerfile -t backup-manager-upk-proof:0.1.0-amd64 .
#
# Three stages: build the standalone frontend, build the standalone backend, then a
# distroless runtime with nothing else in it — no shell, no package manager, matching
# docs/EPIC-B-multi-nas.md §39's container-hardening guidance ("contain no compiler/build
# chain", "expose no shell tooling beyond operational necessity").

FROM node:20-alpine AS frontend-build
WORKDIR /src
COPY apps/ugos/frontend/upk-proof/package.json apps/ugos/frontend/upk-proof/package-lock.json ./
RUN npm ci
COPY apps/ugos/frontend/upk-proof/ ./
RUN npm run build

FROM golang:1.27-alpine AS backend-build
WORKDIR /src
COPY apps/ugos/backend/go.mod ./
COPY apps/ugos/backend/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/backup-manager-upk-proof .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=backend-build /out/backup-manager-upk-proof /app/backup-manager-upk-proof
COPY --from=frontend-build /src/dist /app/www

ENV LISTEN_ADDR=:8080
ENV WEB_ROOT=/app/www
EXPOSE 8080

ENTRYPOINT ["/app/backup-manager-upk-proof"]
