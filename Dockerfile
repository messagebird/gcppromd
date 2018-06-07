FROM alpine:latest
LABEL maintainer="MessageBird <support@messagebird.com>"

RUN apk --no-cache --update add ca-certificates file && rm -rf /var/cache/apk/*
COPY bin/gcppromd-*.linux-amd64 /usr/bin/gcppromd

EXPOSE 8080
ENTRYPOINT ["/usr/bin/gcppromd"]
