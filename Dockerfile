FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
# 빌드 머신에서 실행되는 Go로 대상 노드 아키텍처용 바이너리를 만든다.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /collector .

FROM scratch
# 빈 실행 이미지에서도 AWS HTTPS 연결을 검증할 수 있도록 CA 인증서를 복사한다.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /collector /collector
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/collector"]
