FROM golang:1.26-alpine AS build
ENV GOPROXY=https://goproxy.cn,direct CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -o /out/cloud ./cmd/cloud

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/cloud /app/cloud
COPY web/dist /app/web/dist
ENV PORT=7860
EXPOSE 7860
ENTRYPOINT ["/app/cloud"]
