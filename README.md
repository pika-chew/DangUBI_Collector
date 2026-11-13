# DangUBI Collector

Collector는 SQS에서 S3 객체 생성 알림을 받고, Firehose가 만든 `.jsonl.gz`를 OpenStack Swift에 보관하는 Go 워커입니다. CronJob은 Swift에 저장된 파일을 읽어 MySQL 적재를 담당합니다.

## 처리 순서

1. `worker.go`의 `poll`이 SQS에서 한 번에 메시지 1건을 받습니다. 롱 폴링은 최대 20초이고, 처리 중 메시지 숨김 시간은 120초로 설정한 뒤 60초마다 연장합니다.
2. `s3_input.go`의 `process`가 알림에서 S3 객체 키를 URL 디코딩합니다. 설정한 버킷의 `firehose/processed/` 아래 `.jsonl.gz`만 읽으며, 알림에 버전 ID가 있으면 해당 버전을 받습니다. S3 설정 확인용 `s3:TestEvent`는 객체 복사 없이 처리합니다.
3. `swift.go`가 GZIP 바이트를 그대로 업로드합니다. 날짜 경로와 파일 이름은 S3 객체 키와 같습니다. 예를 들어 S3 키가 `firehose/processed/year=2026/month=11/day=11/example.jsonl.gz`면, 설정한 Swift 컨테이너 안에도 이 이름으로 저장합니다. 다운로드한 파일을 CronJob에서 압축 해제할 수 있도록 객체의 Content-Type을 `application/gzip`으로 설정합니다.
4. 같은 Swift 객체 이름이 이미 있으면 S3 원본 식별자, 길이, 내용의 ETag를 비교합니다. 모두 일치하면 재전달로 처리합니다. 기존 파일과 다르거나 업로드에 실패하면 오류를 반환해 SQS 메시지를 남깁니다. 알림에 포함된 객체가 모두 저장됐을 때 SQS 메시지를 삭제합니다.

## 테스트 큐에서 실행

Go 1.25 이상, 테스트용 SQS 큐와 S3 버킷, 이미 만들어 둔 Swift 컨테이너가 필요합니다. AWS는 SDK 기본 자격 증명 체인을 사용하고, Swift는 Keystone v3 Application Credential로 인증합니다.

| 환경 변수 | 용도 |
|---|---|
| `COLLECTOR_SQS_QUEUE_URL`, `COLLECTOR_S3_BUCKET` | 테스트 큐 URL과 원본 버킷 이름 |
| `AWS_REGION` | AWS 리전. AWS 프로필에서도 지정 가능 |
| `COLLECTOR_SWIFT_CONTAINER` | 대상 Swift 컨테이너 이름 |
| `OS_AUTH_URL` | Keystone v3 인증 URL |
| `OS_APPLICATION_CREDENTIAL_ID`, `OS_APPLICATION_CREDENTIAL_SECRET` | Swift 접근 권한이 있는 Application Credential. Pod에서는 Secret으로 전달 |
| `OS_REGION_NAME`, `OS_INTERFACE` | Swift 서비스 카탈로그의 리전과 인터페이스. 인터페이스 기본값은 `public` |
| `COLLECTOR_HEALTH_ADDR` | 헬스 체크 주소. 기본값은 `:8080` |
| `TMPDIR` | 업로드용 임시 디렉터리. Linux 컨테이너 기본값은 `/tmp` |

```sh
go run . --worker
```

SQS에 `ReceiveMessage`, `DeleteMessage`, `ChangeMessageVisibility` 권한이 필요합니다. S3 객체에는 `GetObject` 또는 버전 조회 시 `GetObjectVersion` 권한이 필요합니다. Swift 자격 증명에는 대상 컨테이너와 객체의 HEAD 조회 및 객체 PUT 권한이 필요합니다.

테스트 버킷의 `firehose/processed/` 아래에 GZIP 파일을 올려 SQS 알림을 보냅니다. 샘플 파일은 `gzip -c sample.jsonl > example.jsonl.gz`로 만들 수 있습니다. Swift에 같은 키가 생기고 파일 바이트가 일치하는지 확인합니다.

## Docker 이미지 빌드 및 GHCR 게시

`DangUBI_Collector` 디렉터리에서 Docker가 실행 중인지 확인합니다. 로컬에서 게시하려면 `pika-chew` 조직에 패키지를 게시할 권한이 있는 GitHub 계정과 `write:packages` 범위의 PAT(classic)이 필요합니다. 조직이 SSO를 사용하면 토큰에도 해당 조직의 SSO를 승인합니다. `GHCR_USER`와 `GHCR_TOKEN`은 셸 환경에서 설정합니다. [GHCR 인증 및 게시](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)

```sh
printf '%s' "$GHCR_TOKEN" | docker login ghcr.io -u "$GHCR_USER" --password-stdin

IMAGE=ghcr.io/pika-chew/dangubi-be-collector:0.1.0-local.1
docker buildx build --platform linux/amd64,linux/arm64 --tag "$IMAGE" --push .
docker buildx imagetools inspect "$IMAGE"
```

`--push`는 두 아키텍처용 이미지를 빌드해 GHCR로 바로 게시합니다. 로컬 이미지로만 확인하려면 `docker build --tag "$IMAGE" .`을 실행합니다. 최초 게시한 GHCR 패키지는 기본적으로 비공개입니다. 비공개 이미지를 클러스터에서 가져오려면 해당 패키지를 읽을 수 있는 `read:packages` 토큰으로 레지스트리 Secret을 만들고 `imagePullSecrets`에 지정합니다. [Docker 멀티 플랫폼 빌드](https://docs.docker.com/build/building/multi-platform/)

기본 빌더가 멀티 플랫폼 빌드를 지원하지 않으면 아래 빌더를 한 번 생성한 뒤 빌드 명령에 `--builder collector-builder`를 추가합니다.

```sh
docker buildx create --name collector-builder --driver docker-container
```

`0.1.0-local.1`은 검증용 예시 태그입니다. 컨테이너를 실행할 때 인자로 `--worker`를 전달합니다.

## Kubernetes 헬스 체크

`/livez`는 HTTP 서버가 응답하면 `200`을 반환합니다. `/readyz`는 최근 SQS 수신 요청과 Swift 컨테이너 조회가 성공했을 때 `200`을, 그 외에는 `503`을 반환합니다. 시작 시에도 Swift에 인증하고 컨테이너에 접근할 수 있는지 확인합니다.

```yaml
args: ["--worker"]
ports:
  - name: health
    containerPort: 8080
startupProbe:
  httpGet:
    path: /livez
    port: health
  periodSeconds: 5
  failureThreshold: 30
livenessProbe:
  httpGet:
    path: /livez
    port: health
readinessProbe:
  httpGet:
    path: /readyz
    port: health
  timeoutSeconds: 3
```

워커는 한 번에 한 객체를 디스크에 임시 저장하고 업로드가 끝나면 파일을 정리합니다. 컨테이너 루트 파일시스템을 읽기 전용으로 쓰는 경우 `/tmp` 또는 `TMPDIR`에 쓰기 가능한 볼륨을 마운트합니다.
