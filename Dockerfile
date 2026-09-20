# 9Drive - multi-stage: builds frontend + backend inside Docker
FROM node:24-alpine AS frontend
WORKDIR /build/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm install
COPY frontend/ ./
RUN npm run build

FROM golang:1.26-alpine AS backend
WORKDIR /build/backend-go
RUN apk add --no-cache git
COPY backend-go/go.mod backend-go/go.sum ./
RUN go mod download
COPY backend-go/ ./
# Copy frontend build for embedding
COPY --from=frontend /build/frontend/dist ./dist
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.buildVersion=${VERSION}" -o /9drive .

FROM scratch
COPY --from=backend /9drive /9drive
ENV APP_PORT=4000 \
    DATABASE_URL=file:/data/9drive.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)
VOLUME /data
EXPOSE 4000
ENTRYPOINT ["/9drive"]
