FROM golang:1-alpine AS build-env

ENV REPO_PATH=$GOPATH/github.com/messagebird/gcppromd
ENV GO111MODULE=on
ENV CGO_ENABLED=0

ADD ./ $REPO_PATH
WORKDIR $REPO_PATH
RUN go build \
    -mod=vendor \
    -ldflags "-s -w" \
    -o /gcppromd $REPO_PATH/cmd/gcppromd/

FROM gcr.io/distroless/static:nonroot

COPY --from=build-env /gcppromd /bin/gcppromd

ENTRYPOINT ["/bin/gcppromd"]

LABEL name=gcppromd
LABEL version=$VERSION
LABEL maintainer="MessageBird B.V. Infrastructure Team <infrastructure-networking@messagebird.com>"
