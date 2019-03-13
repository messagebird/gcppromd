FROM golang:1-alpine AS build-env

ENV REPO_PATH=$GOPATH/github.com/messagebird/gcppromd

ENV GO111MODULE=on
ENV CGO_ENABLED=0
ENV GOOS=linux

ADD ./ $REPO_PATH

RUN ( \
        cd $REPO_PATH && \
        go build -mod=vendor -o gcppromd $REPO_PATH/cmd/gcppromd/ && \
        mv ./gcppromd / ; \
    )

FROM alpine:3.9

COPY --from=build-env /gcppromd /bin/gcppromd

RUN ( \
        apk add --no-cache \
            ca-certificates \
            file && \
        mkdir -p /etc/prom_sd/ && \
        chown nobody: /etc/prom_sd/ \
    )

USER nobody:nogroup
EXPOSE 8080

ENTRYPOINT ["/bin/gcppromd"]

LABEL name=gcppromd
LABEL version=$VERSION
LABEL maintainer="MessageBird B.V. Infrastructure Team <infrastructure-networking@messagebird.com>"
