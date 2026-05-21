FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0
ENV GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/mindgraph ./cmd/server

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /out/mindgraph /mindgraph

EXPOSE 8080
USER nonroot
ENTRYPOINT ["/mindgraph"]
