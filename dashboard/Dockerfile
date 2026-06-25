FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o dashboard-app ./dashboard/main.go

FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/dashboard-app .
COPY --from=builder /app/dashboard/index.html .
EXPOSE 3000
CMD ["./dashboard-app"]
