# wax server image: `docker compose up -d` builds and runs it with
# PostgreSQL. The program is static; the runtime image holds nothing else
# (CA certificates included) and runs as an unprivileged user.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.buildVersion=${VERSION}" -o /out/wax ./cmd/wax

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/wax /usr/local/bin/wax
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/wax"]
CMD ["serve", "-listen", "0.0.0.0:8080"]
