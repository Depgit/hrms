# Builds the HRMS API into a small image. One binary, `hrms`; the default command is `serve`.
# One-time setup commands (hrms migrate, hrms seed, hrms create-admin) run from the same image with another command.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hrms ./cmd/hrms

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 app
USER app
COPY --from=build /out/hrms /usr/local/bin/hrms
ENV TZ=Asia/Kolkata APP_TZ=Asia/Kolkata
EXPOSE 8080
ENTRYPOINT ["hrms"]
CMD ["serve"]
