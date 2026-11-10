# DangUBI Collector

Go로 만든 Collector의 로컬 검증 버전입니다. JSONL에서 차량 레코드를 한 줄씩 읽고, `collector_events`와 `outbox`에 함께 기록합니다. 현재 프로그램은 파일 경로를 인자로 받아 처리한 뒤 종료합니다. SQS 수신과 S3 다운로드는 구현 전입니다.

## Go를 선택한 이유

Collector는 SQS를 폴링하고, S3의 압축 파일을 스트리밍으로 읽고, MySQL에 기록하는 백그라운드 워커입니다. AWS 연결에는 AWS SDK for Go v2를, GZIP·JSON 처리에는 Go 표준 라이브러리를 사용할 수 있습니다. DB 쓰기는 `database/sql`의 트랜잭션으로 두 INSERT와 커밋 경계를 코드에서 확인할 수 있습니다.

실행 파일 하나로 컨테이너에 담을 수 있고, `linux/amd64`와 `linux/arm64`용 빌드도 같은 Dockerfile로 만들 수 있습니다.

## 로컬 실행

Go 1.25 이상과 로컬 MySQL이 필요합니다. 기존 운영 데이터와 분리된 `collector_dev` DB와 해당 DB에 접근 가능한 로컬 계정을 준비합니다.

```sh
mysql -h 127.0.0.1 -u collector -p collector_dev < schema.sql
export COLLECTOR_MYSQL_DSN='collector:local-password@tcp(127.0.0.1:3306)/collector_dev'
go run . sample.jsonl
go run . sample.jsonl
```

빈 DB에서 같은 파일을 두 번 실행한 뒤 아래 두 쿼리는 각각 `1`을 반환합니다.

```sql
SELECT COUNT(*) FROM collector_events;
SELECT COUNT(*) FROM outbox;
```

## 코드 읽는 순서

1. `main.go`의 `ingest`가 JSONL을 한 줄씩 읽고 `parseRecord`가 샘플 형식을 검사합니다. 빈 줄은 건너뛰고 한 줄은 1 MiB 미만이어야 합니다.
2. `store.go`의 `saveRecord`가 레코드마다 트랜잭션을 엽니다. `idempotencyKey`가 이미 기록됐다면 건너뜁니다. 새 레코드는 두 INSERT가 성공한 뒤에 커밋합니다.
3. 오류가 발생하면 그 줄에서 멈추고 현재 트랜잭션을 롤백합니다. 이전 줄은 이미 커밋됐으므로 같은 파일로 다시 실행할 수 있습니다.

`schema.sql`과 `collector_demo` 이벤트는 로컬 검증용입니다.

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

`0.1.0-local.1`은 검증용 예시 태그입니다. GitOps의 `0.1.0` 태그는 상시 실행 워커가 준비되면 사용합니다. 현재 이미지를 실행하려면 입력 JSONL을 컨테이너에 마운트하고 그 경로와 `COLLECTOR_MYSQL_DSN`을 전달해야 합니다.

## Kubernetes 통합 검증

현재 이미지로는 테스트 DB를 대상으로 파일 적재를 확인할 수 있습니다. SQS부터 MySQL까지 확인하려면 먼저 SQS·S3 수신 코드를 구현하고, 실제 큐 메시지 형식과 레코드 고유 키, 운영 테이블과 outbox 이벤트 계약을 맞춥니다.

Deployment에는 큐 URL, SQS 수신·삭제 및 메시지 숨김 시간 연장 권한, S3 읽기 권한을 설정합니다. DB는 MySQL Router의 쓰기 주소 `mysql-router.dangubi-data.svc.cluster.local:3306`과 DB 계정으로 연결하고, Kubernetes probe는 Go 헬스 체크 경로에 맞춥니다.

그다음 새 고유 키로 테스트 입력을 한 건 보내 Pod 로그에서 수신과 처리를 확인하고, 그 키로 업무 테이블과 outbox를 조회합니다. SQS 메시지는 DB 커밋이 끝난 뒤 삭제되는지 확인합니다.
