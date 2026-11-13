// DangUBI Collector CI/CD 파이프라인 (Multibranch Pipeline + GitHub Branch Source)
//
// 실행 범위 (develop 없이 작업 브랜치 → main으로 바로 머지)
//   - 작업 브랜치 → main PR : Secret Scan → Lint → Test → Build → SCA
//   - main 머지             : 위 단계 + Image Build & Scan → Push to GHCR → Update GitOps
//
// [중요]
// 어떤 브랜치/PR에서 빌드가 시작될지는 이 파일이 아니라 Jenkins Multibranch Job의
// Branch Sources 설정이 정한다. 브랜치는 main만, PR은 "타깃과 머지한 상태"로 탐색하도록 설정한다.
// (작업 브랜치 push마다 빌드가 돌지 않게 하고, PR 빌드는 main에 머지됐을 때의 코드를 검사한다)
//
// 빌드 구조
//   Dockerfile이 멀티스테이지로 직접 컴파일한다 (golang 빌드 스테이지 → scratch 실행 이미지로 바이너리만 복사).
//   그래서 CI의 Build 단계는 이미지에 쓰이지 않는다. Dockerfile과 같은 옵션으로 컴파일되는지 확인하고
//   SCA 대상 바이너리를 만드는 용도다. 실제 배포 바이너리는 main의 Image Build & Scan에서 다시 검사한다.
//
// 배포 방식
//   main 빌드는 이미지를 GHCR에 push한 뒤, GitOps 레포 kustomization.yaml의 images 항목을
//   새 태그·digest로 바꿔 main에 push한다. 클러스터 반영은 Argo CD(dangubi-collector)가 한다.
//   GitOps 매니페스트에 images 항목이 아직 없으면 GitOps 갱신만 건너뛰고 UNSTABLE로 끝낸다
//   (이미지는 push됨, Slack에 이미지 주소를 보낸다).
//
// Jenkins에는 docker CLI만 있으므로, 도구는 단계마다 공식 컨테이너 이미지로 실행한다.
// (docker.image().inside는 Jenkins 컨테이너의 볼륨을 그대로 공유하므로 워크스페이스 경로가 같다)
//
// 사전 준비
//  - Jenkins Credentials: GHCR_CRED_ID (Username with password, password = GitHub PAT, 권한 write:packages)
//  - Jenkins Credentials: GITOPS_CRED_ID (Username with password, password = DangUBI_GitOps contents 쓰기 권한 토큰)
//  - GitOps 레포 GITOPS_FILE에 images 항목을 만들어 두면 그때부터 자동 반영된다. Deployment의 image는 name과 같은 값으로 참조한다.
//        images:
//          - name: dangubi-collector
//            newName: ghcr.io/pika-chew/dangubi-collector
//            newTag: "initial"
//    newName·newTag는 이 파이프라인이 바꾸고, digest 줄은 newTag 바로 아래에 새로 쓴다 (없으면 추가).
//  - GHCR 패키지가 private이면 클러스터 dangubi 네임스페이스에 GHCR pull용 imagePullSecret이 필요하다.
//  - Slack: Manage Jenkins → System → Slack에 워크스페이스와 Bot 토큰을 등록하고, 봇을 SLACK_CHANNEL에 초대한다.
//
// Slack 알림 (빌드가 끝났을 때 결과만 한 번 보낸다)
//  - main : 모든 결과 — 성공, GitOps 반영 보류(UNSTABLE), GitOps 갱신 실패, 실패, 중단
//  - PR   : 실패, 정상화(fixed)

pipeline {
    agent any

    options {
        disableConcurrentBuilds()                 // 같은 브랜치 빌드가 겹치지 않게 (GitOps push 충돌 방지)
        timestamps()
        timeout(time: 20, unit: 'MINUTES')
        buildDiscarder(logRotator(numToKeepStr: '30'))
    }

    environment {
        APP_NAME        = 'dangubi-collector'

        // ===== main 이미지 릴리스 =====
        REGISTRY        = 'ghcr.io'
        IMAGE_REPO      = "${REGISTRY}/pika-chew/dangubi-collector"        // GHCR 이미지 이름은 소문자만 가능
        IMAGE_PLATFORM  = 'linux/amd64'                                  // 클러스터 노드 아키텍처
        GHCR_CRED_ID    = 'ghcr-push'                                    // Jenkins Credentials ID

        // ===== GitOps 갱신 (main에 직접 push) =====
        GITOPS_REPO_URL = 'https://github.com/pika-chew/DangUBI_GitOps.git'
        GITOPS_BRANCH   = 'main'
        GITOPS_FILE     = 'manifests/workloads/collector/kustomization.yaml'
        KUSTOMIZE_IMAGE = 'dangubi-collector'                              // 그 파일 images 항목의 name
        GITOPS_CRED_ID  = 'github-gitops-push'                           // Jenkins Credentials ID

        // Go 모듈·빌드 캐시 볼륨을 main과 PR로 분리. PR 코드가 캐시에 남긴 내용이
        // main 빌드에 섞이지 않도록 한다
        GO_CACHE        = "${env.BRANCH_NAME == 'main' ? 'ci-go-cache-collector-main' : 'ci-go-cache-collector-ci'}"

        // 도구 컨테이너용 네트워크 (MTU 1450). 기본 브리지(MTU 1500)는 VM NIC(1450)보다 커서
        // 외부 HTTPS 핸드셰이크가 멈춘다 (Go 모듈, Trivy DB 다운로드가 timeout)
        CI_NETWORK      = 'ci-net-mtu1450'

        SLACK_CHANNEL   = '#jenkins'
        REPORT_DIR      = 'reports'

        // 도구 이미지 (버전을 고정해 빌드를 재현 가능하게 유지)
        // GO_IMAGE는 Dockerfile 빌드 스테이지(golang:1.26-alpine)와 같은 마이너 버전.
        // -race 테스트에 gcc가 필요해서 alpine이 아닌 Debian 기반 이미지를 쓴다
        GO_IMAGE        = 'golang:1.26'
        GOTESTSUM_PKG   = 'gotest.tools/gotestsum@v1.12.0'               // go test 결과를 JUnit XML로 변환
        DOCKER_IMAGE    = 'docker:27.3.1-cli'
        TRIVY_IMAGE     = 'aquasec/trivy:0.58.1'
        GITLEAKS_IMAGE  = 'zricethezav/gitleaks:v8.21.2'
    }

    stages {
        stage('Prepare') {
            steps {
                script {
                    env.IMAGE_TAG = env.GIT_COMMIT.take(7)
                    env.COMMIT_SUBJECT = sh(script: 'git log -1 --format=%s', returnStdout: true).trim()
                    env.COMMIT_AUTHOR = sh(script: 'git log -1 --format=%an', returnStdout: true).trim()
                    if (env.CHANGE_ID) {
                        env.BUILD_KIND = "PR #${env.CHANGE_ID}: ${env.CHANGE_BRANCH} → ${env.CHANGE_TARGET}"
                    } else {
                        env.BUILD_KIND = "Push/Merge: ${env.BRANCH_NAME}"
                    }
                    currentBuild.description = "${env.BUILD_KIND} (${env.IMAGE_TAG})"
                }
                sh 'mkdir -p "$REPORT_DIR"'

                // 이미 있으면 그대로 쓰고, 다른 빌드가 동시에 만들어도 통과
                sh '''
                    docker network inspect "$CI_NETWORK" > /dev/null 2>&1 \
                        || docker network create --opt com.docker.network.driver.mtu=1450 "$CI_NETWORK" > /dev/null \
                        || docker network inspect "$CI_NETWORK" > /dev/null
                '''

                // Go 캐시 볼륨 (모듈·컴파일 결과를 빌드마다 다시 받거나 만들지 않도록 공유)
                // 새 볼륨은 root 소유라 빌드 컨테이너(jenkins 사용자)가 쓸 수 있게 소유자를 맞춘다
                sh '''
                    docker volume create "$GO_CACHE" > /dev/null
                    docker run --rm -u 0 -v "$GO_CACHE:/cache" "$GO_IMAGE" chown "$(id -u):$(id -g)" /cache
                '''
            }
            post { failure { recordFailedStage() } }
        }

        // 찾은 비밀값은 리포트에 가려서(--redact) 남긴다
        stage('Secret Scan') {
            steps {
                inContainer(env.GITLEAKS_IMAGE) {
                    sh 'gitleaks detect --source . --no-banner --redact --report-path "$REPORT_DIR/gitleaks.json"'
                }
            }
            post { failure { recordFailedStage() } }
        }

        // 1. gofmt 형식 위반 파일 목록과 2. go vet 결과를 남긴다
        stage('Lint') {
            steps {
                withGo {
                    sh '''
                        unformatted=$(gofmt -l .)
                        if [ -n "$unformatted" ]; then
                            echo "$unformatted" > "$REPORT_DIR/gofmt.txt"
                            echo "gofmt가 필요한 파일:"; echo "$unformatted"
                            exit 1
                        fi
                        go vet ./...
                    '''
                }
            }
            post { failure { recordFailedStage() } }
        }

        // 테스트는 SQS·S3·Swift를 모두 mock으로 대신해서 외부 인프라가 필요 없다
        stage('Test') {
            steps {
                withGo {
                    sh 'go run "$GOTESTSUM_PKG" --junitfile "$REPORT_DIR/junit.xml" -- -race -count=1 ./...'
                }
            }
            post {
                always {
                    script {
                        // 실패 알림에 붙일 테스트 개수 (컴파일 실패 등으로 결과 파일이 없으면 null)
                        def result = junit allowEmptyResults: true, testResults: "${env.REPORT_DIR}/junit.xml"
                        if (result) {
                            env.TEST_FAIL_COUNT = result.failCount.toString()
                            env.TEST_TOTAL_COUNT = result.totalCount.toString()
                        }
                    }
                }
                failure { recordFailedStage() }
            }
        }

        // Dockerfile과 같은 옵션으로 컴파일. 이 바이너리는 SCA 대상이고 이미지에는 들어가지 않는다
        stage('Build') {
            steps {
                withGo {
                    sh 'CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o build/collector .'
                }
            }
            post { failure { recordFailedStage() } }
        }

        // Go 바이너리에는 모듈 버전과 Go 버전이 들어 있어서, 의존성과 표준 라이브러리 취약점을 함께 찾는다
        stage('SCA') {
            steps {
                inContainer(env.TRIVY_IMAGE) {
                    trivyScan('trivy-sca.txt', 'rootfs --scanners vuln --severity HIGH,CRITICAL build')
                }
            }
            post { failure { recordFailedStage() } }
        }

        // ───────────────────── main 이미지 릴리스 ─────────────────────
        stage('Release') {
            when { branch 'main' }
            stages {
                stage('Image Build & Scan') {
                    steps {
                        withDocker {
                            // source 라벨: GHCR 패키지를 이 레포에 연결한다 (패키지 페이지·권한 상속)
                            sh '''
                                docker build --pull --platform "$IMAGE_PLATFORM" \
                                    --label org.opencontainers.image.source=https://github.com/pika-chew/DangUBI_Collector \
                                    --label org.opencontainers.image.revision="$GIT_COMMIT" \
                                    -t "$IMAGE_REPO:$IMAGE_TAG" .
                            '''
                        }
                        // scratch 이미지라 OS 패키지는 없고, 이미지 안의 Go 바이너리를 검사한다
                        inContainer(env.TRIVY_IMAGE, dockerSocketArgs()) {
                            trivyScan('trivy-image.txt', 'image --severity CRITICAL --ignore-unfixed "$IMAGE_REPO:$IMAGE_TAG"')
                        }
                    }
                    post { failure { recordFailedStage() } }
                }

                stage('Push to GHCR') {
                    steps {
                        // 로그인 정보는 워크스페이스 안의 DOCKER_CONFIG에 두고, 빌드가 끝나면 cleanWs로 지운다
                        withEnv(["DOCKER_CONFIG=${env.WORKSPACE}/.docker"]) {
                            withCredentials([usernamePassword(credentialsId: env.GHCR_CRED_ID,
                                                              usernameVariable: 'GHCR_USER', passwordVariable: 'GHCR_TOKEN')]) {
                                withDocker {
                                    sh '''
                                        mkdir -p "$DOCKER_CONFIG"
                                        echo "$GHCR_TOKEN" | docker login "$REGISTRY" -u "$GHCR_USER" --password-stdin
                                        docker push "$IMAGE_REPO:$IMAGE_TAG"
                                        docker logout "$REGISTRY"
                                    '''
                                    script {
                                        // GHCR 태그는 덮어쓸 수 있으므로, 배포는 digest로 고정한다
                                        def repoDigest = sh(script: 'docker inspect --format "{{index .RepoDigests 0}}" "$IMAGE_REPO:$IMAGE_TAG"',
                                                            returnStdout: true).trim()
                                        env.IMAGE_DIGEST = repoDigest.substring(repoDigest.indexOf('@') + 1)
                                        currentBuild.description = "${env.IMAGE_REPO}:${env.IMAGE_TAG}@${shortDigest(env.IMAGE_DIGEST)}"
                                    }
                                }
                            }
                        }
                    }
                    post { failure { recordFailedStage() } }
                }

                stage('Update GitOps') {
                    steps {
                        // try/catch는 Groovy 코드라 Declarative steps 안에서는 script 블록이 필요하다
                        script {
                            try {
                                syncGitOpsImage()
                            } catch (e) {
                                env.GITOPS_SYNC_FAILED = 'true'
                                throw e
                            }
                        }
                    }
                    post { failure { recordFailedStage() } }
                }
            }
        }
    }

    post {
        always {
            archiveArtifacts artifacts: "${REPORT_DIR}/**", allowEmptyArchive: true
            sh 'if [ -n "$IMAGE_TAG" ]; then docker image rm "$IMAGE_REPO:$IMAGE_TAG" > /dev/null 2>&1 || true; fi'   // Jenkins 서버 디스크 정리
        }
        // ── main: 결과마다 한 번 ──
        success {
            script {
                if (env.BRANCH_NAME == 'main') {
                    def result = env.GITOPS_CHANGED == 'true'
                        ? "GitOps에 `${env.IMAGE_TAG}` · `${shortDigest(env.IMAGE_DIGEST)}` 반영 (Argo CD 동기화 후 롤링 업데이트)"
                        : "GitOps가 이미 같은 digest를 가리킴 (변경 없음)"
                    notifyMain('good', "✅ *main 성공* · ${result}")
                }
            }
        }
        unstable {
            script {
                if (env.BRANCH_NAME == 'main' && env.GITOPS_PENDING == 'true') {
                    notifyMain('warning', "🟡 *main 이미지 push 완료 · GitOps 반영 보류* · `${env.GITOPS_FILE}`에 images name `${env.KUSTOMIZE_IMAGE}` 항목이 아직 없음\n" +
                        "`${env.IMAGE_REPO}:${env.IMAGE_TAG}@${env.IMAGE_DIGEST}`")
                }
            }
        }
        failure {
            script {
                if (env.BRANCH_NAME == 'main' && env.GITOPS_SYNC_FAILED == 'true') {
                    notifyMain('danger', "🔴 *main GitOps 갱신 실패* · 이미지는 push됨. `${env.GITOPS_FILE}`의 `${env.KUSTOMIZE_IMAGE}`를 직접 맞춰 주세요:\n" +
                        "`${env.IMAGE_REPO}:${env.IMAGE_TAG}@${env.IMAGE_DIGEST}`")
                } else if (env.BRANCH_NAME == 'main') {
                    notifyMain('danger', "🔴 *main 실패* · 배포된 이미지는 바뀌지 않음\n${failureDetail()}")
                } else if (env.CHANGE_ID) {
                    slackSend(channel: env.SLACK_CHANNEL, color: 'danger', message: "🔴 *${APP_NAME}* ${prLine()} 검사 실패\n${failureDetail()}")
                }
            }
        }
        aborted {
            script {
                if (env.BRANCH_NAME == 'main') {
                    notifyMain('#999999', "⚪ *main 중단* · GitOps는 바뀌지 않았을 수 있음 · ${buildLink('console', 'Console Output')}")
                }
            }
        }
        // ── PR: 실패하던 검사가 다시 통과 ──
        fixed {
            script {
                if (env.CHANGE_ID) {
                    slackSend(channel: env.SLACK_CHANNEL, color: 'good', message: "✅ *${APP_NAME}* ${prLine()} 검사 통과 (이전 실패 해결) · ${buildLink()}")
                }
            }
        }
        cleanup { cleanWs() }
    }
}

// ───────────────────────── helpers ─────────────────────────

// 이미지의 ENTRYPOINT(trivy, gitleaks 등)를 비우고 셸로 실행한다. HOME은 jenkins 사용자가 쓸 수 있는 곳으로 둔다.
// 외부 다운로드가 멈추지 않도록 MTU 1450 네트워크(CI_NETWORK, Prepare에서 생성)로 띄운다.
def inContainer(String image, String args = '', Closure body) {
    docker.image(image).inside("--entrypoint= -e HOME=/tmp --network ${env.CI_NETWORK} ${args}") { body() }
}

// 호스트 Docker 데몬을 쓰는 컨테이너 인자 (docker.sock 그룹 권한 포함)
def dockerSocketArgs() {
    def gid = sh(script: 'stat -c %g /var/run/docker.sock', returnStdout: true).trim()
    return "-v /var/run/docker.sock:/var/run/docker.sock --group-add ${gid}"
}

def withDocker(Closure body) {
    inContainer(env.DOCKER_IMAGE, dockerSocketArgs(), body)
}

// Go 컨테이너에서 실행한다. Jenkins 서버에 Go를 따로 설치하지 않아도 된다.
// GOTOOLCHAIN=local: go.mod 요구 버전이 이미지보다 높으면 몰래 새 Go를 받지 않고 실패하게 한다
def withGo(Closure body) {
    inContainer(env.GO_IMAGE, "-v ${env.GO_CACHE}:/go-cache -e GOMODCACHE=/go-cache/mod -e GOCACHE=/go-cache/build -e GOTOOLCHAIN=local") { body() }
}

// GitOps 레포를 얕게 받아 GITOPS_FILE의 images 항목(name = KUSTOMIZE_IMAGE)만 바꿔 main에 push한다.
//  - images 항목이 아직 없으면(매니페스트 작성 전) 건너뛰고 UNSTABLE로 표시한다
//  - newName·newTag는 값만 바꾸고, digest는 기존 줄을 지운 뒤 newTag 바로 아래에 새로 쓴다 (주석·다른 항목은 그대로)
//  - 태그는 따옴표로 감싼다. 짧은 SHA가 숫자로만 되면 YAML이 숫자로 읽기 때문
//  - 항목은 있는데 newName·newTag 줄을 못 찾으면 조용히 넘어가지 않고 실패
//  - 다른 파이프라인이 그사이 main에 push했다면 rebase 후 다시 시도한다
def syncGitOpsImage() {
    def gitopsDir = "${env.WORKSPACE}/.gitops"
    withCredentials([usernamePassword(credentialsId: env.GITOPS_CRED_ID, usernameVariable: 'GIT_USER', passwordVariable: 'GIT_TOKEN')]) {
        // 토큰을 URL이나 .git/config에 남기지 않도록 askpass로 전달한다
        writeFile file: '.git-askpass.sh', text: '#!/bin/sh\ncase "$1" in Username*) echo "$GIT_USER" ;; *) echo "$GIT_TOKEN" ;; esac\n'
        withEnv(["GIT_ASKPASS=${env.WORKSPACE}/.git-askpass.sh", "GIT_TERMINAL_PROMPT=0", "GITOPS_DIR=${gitopsDir}"]) {
            sh '''
                chmod 700 .git-askpass.sh
                rm -rf "$GITOPS_DIR"
                git clone -q --depth 20 --branch "$GITOPS_BRANCH" "$GITOPS_REPO_URL" "$GITOPS_DIR"
            '''

            def hasEntry = sh(script: '''
                cd "$GITOPS_DIR"
                awk -v name="$KUSTOMIZE_IMAGE" '
                    /^[^ #]/                  { in_images = /^images:/ }
                    in_images && /^ *- name:/ { v = $0; sub(/^ *- name: */, "", v); gsub(/[" ]/, "", v); if (v == name) found = 1 }
                    END                       { exit !found }
                ' "$GITOPS_FILE"
            ''', returnStatus: true) == 0
            if (!hasEntry) {
                env.GITOPS_PENDING = 'true'
                unstable("${env.GITOPS_FILE}에 images name '${env.KUSTOMIZE_IMAGE}' 항목이 없어 GitOps 갱신을 건너뜁니다.")
                return
            }

            sh '''
                cd "$GITOPS_DIR"
                awk -v name="$KUSTOMIZE_IMAGE" -v repo="$IMAGE_REPO" -v tag="$IMAGE_TAG" -v digest="$IMAGE_DIGEST" -v q='"' '
                    /^[^ #]/                              { in_images = /^images:/; target = 0 }
                    in_images && /^ *- name:/             { v = $0; sub(/^ *- name: */, "", v); gsub(/[" ]/, "", v); target = (v == name) }
                    in_images && target && /^ *digest:/   { next }
                    in_images && target && /^ *newName:/  { sub(/newName:.*/, "newName: " repo); n++ }
                    in_images && target && /^ *newTag:/   {
                        sub(/newTag:.*/, "newTag: " q tag q); t++
                        print
                        indent = $0; sub(/[^ ].*/, "", indent)
                        print indent "digest: " digest
                        next
                    }
                    { print }
                    END { if (n != 1 || t != 1) exit 1 }
                ' "$GITOPS_FILE" > "$GITOPS_FILE.tmp" \
                    || { echo "$GITOPS_FILE 에서 images name '$KUSTOMIZE_IMAGE'의 newName/newTag를 찾지 못함"; exit 1; }
                mv "$GITOPS_FILE.tmp" "$GITOPS_FILE"
                git --no-pager diff -- "$GITOPS_FILE"
            '''
            def changed = sh(script: 'cd "$GITOPS_DIR" && ! git diff --quiet -- "$GITOPS_FILE"', returnStatus: true) == 0
            env.GITOPS_CHANGED = changed.toString()
            if (!changed) {
                echo "GitOps 레포가 이미 ${env.IMAGE_DIGEST}를 가리킵니다."
                return
            }
            sh '''
                cd "$GITOPS_DIR"
                git add "$GITOPS_FILE"
                git -c user.name="Jenkins" -c user.email="jenkins@dangubi.local" commit -q \
                    -m "chore: update ${KUSTOMIZE_IMAGE} image to ${IMAGE_TAG}" \
                    -m "Deployed by ${JOB_NAME} #${BUILD_NUMBER} from DangUBI_Collector@${GIT_COMMIT}" \
                    -m "${BUILD_URL}"
                for attempt in 1 2 3; do
                    git push -q origin "HEAD:$GITOPS_BRANCH" && exit 0
                    echo "push 실패 (${attempt}/3) → 최신 main으로 rebase 후 재시도"
                    git pull -q --rebase origin "$GITOPS_BRANCH"
                done
                exit 1
            '''
        }
    }
}

// Trivy 결과를 콘솔과 리포트 파일에 같이 남긴다. 취약점이 있으면 리포트를 출력한 뒤 실패로 끝낸다.
// (--output을 쓰면 콘솔에는 아무것도 나오지 않으므로 파일을 다시 출력한다)
// 경로 변수 이름은 TRIVY_로 시작하면 안 된다. Trivy가 TRIVY_<플래그> 환경변수를 플래그 값으로 읽는다
def trivyScan(String reportName, String args) {
    withEnv(["SCAN_REPORT=${env.REPORT_DIR}/${reportName}"]) {
        sh """
            rc=0
            trivy ${args} --cache-dir .trivy-cache --exit-code 1 --output "\$SCAN_REPORT" || rc=\$?
            [ -f "\$SCAN_REPORT" ] && cat "\$SCAN_REPORT"
            exit \$rc
        """
    }
}

// 실패한 단계 이름을 남긴다. 가장 먼저 실패한 단계만 기록한다.
def recordFailedStage() {
    if (!env.FAILED_STAGE) {
        env.FAILED_STAGE = env.STAGE_NAME
    }
}

// 실패 알림에 붙일 내용: 실패한 단계, 단계별 원인을 볼 수 있는 곳(테스트 결과·스캔 리포트), 콘솔 로그 링크
def failureDetail() {
    def parts = []
    if (env.FAILED_STAGE) {
        parts << "*${env.FAILED_STAGE}* 단계에서 실패"
    }
    def detail = null
    switch (env.FAILED_STAGE) {
        case 'Lint':
            detail = reportLink('gofmt.txt', 'gofmt 미적용 파일 있음') ?: 'go vet 경고 (Console Output 확인)'
            break
        case 'Test':
            if (env.TEST_FAIL_COUNT && env.TEST_FAIL_COUNT != '0') {
                detail = "테스트 ${env.TEST_FAIL_COUNT}개 실패 / 전체 ${env.TEST_TOTAL_COUNT}개 (${buildLink('testReport/', '테스트 결과')})"
            } else {
                detail = "테스트 결과 없음 (컴파일 오류, data race 등)"
            }
            break
        case 'Secret Scan':
            detail = reportLink('gitleaks.json', '비밀값이 커밋에 포함됨')
            break
        case 'SCA':
            detail = reportLink('trivy-sca.txt', '의존성·Go 표준 라이브러리 취약점(HIGH/CRITICAL) 발견')
            break
        case 'Image Build & Scan':
            detail = reportLink('trivy-image.txt', '이미지 취약점(CRITICAL) 발견')
            break
    }
    if (detail) {
        parts << detail
    }
    parts << buildLink('console', 'Console Output')
    return parts.join(' · ')
}

// 리포트 파일이 있을 때만 설명과 링크를 만든다 (스캔 전에 실패했으면 파일이 없다)
def reportLink(String reportName, String label) {
    fileExists("${env.REPORT_DIR}/${reportName}") ? "${label} (${buildLink("artifact/${env.REPORT_DIR}/${reportName}", reportName)})" : null
}

// main 결과 알림. 어떤 커밋의 어느 빌드인지를 함께 붙인다.
def notifyMain(String color, String message) {
    slackSend(channel: env.SLACK_CHANNEL, color: color,
              message: "*${env.APP_NAME}* ${message}\n${commitLine()} · ${buildLink()}")
}

// Slack 링크. path가 없으면 빌드 페이지, 있으면 빌드 아래의 하위 경로(console, artifact/...)
def buildLink(String path = '', String label = null) {
    "<${env.BUILD_URL}${path}|${label ?: "#${env.BUILD_NUMBER}"}>"
}

// Prepare 전에 실패하면 커밋 정보가 없으므로 브랜치 이름으로 대신 표시
def commitLine() {
    env.COMMIT_SUBJECT
        ? "`${env.GIT_COMMIT.take(7)}` ${slackEscape(env.COMMIT_SUBJECT)} (${slackEscape(env.COMMIT_AUTHOR)})"
        : "`${env.BRANCH_NAME}`"
}

def prLine() {
    "<${env.CHANGE_URL}|PR #${env.CHANGE_ID}> ${slackEscape(env.CHANGE_TITLE)} (${slackEscape(env.CHANGE_AUTHOR)})"
}

// sha256:<64자> → sha256:<앞 12자>
def shortDigest(String digest) {
    digest = digest ?: '-'
    digest.length() > 19 ? digest.take(19) : digest
}

// Slack mrkdwn에서 특수 의미가 있는 문자를 바꾼다
def slackEscape(String text) {
    (text ?: '').replace('&', '&amp;').replace('<', '&lt;').replace('>', '&gt;')
}
