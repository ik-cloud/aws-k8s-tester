#!/usr/bin/env bash
set -e

if ! [[ "$0" =~ scripts/aws-k8s-tester.build.container.push.sh ]]; then
  echo "must be run from repository root"
  exit 255
fi

$(aws ecr get-login --no-include-email --region us-west-2)

if [[ -z "${GIT_COMMIT}" ]]; then
  GIT_COMMIT=$(git rev-parse --short=12 HEAD || echo "GitNotFound")
fi

if [[ -z "${RELEASE_VERSION}" ]]; then
  RELEASE_VERSION=$(git rev-parse --short=12 HEAD || echo "GitNotFound")
fi

if [[ -z "${BUILD_TIME}" ]]; then
  BUILD_TIME=$(date -u '+%Y-%m-%d_%H:%M:%S')
fi

_AWS_REGION=us-west-2
if [[ "${AWS_REGION}" ]]; then
  _AWS_REGION=${AWS_REGION}
fi

if [[ -z "${REGISTRY}" ]]; then
  REGISTRY=$(aws-k8s-tester ecr --region=${_AWS_REGION} get-registry)
fi

TAG=`date +%Y%m%d`-${GIT_COMMIT}
echo "Building:" ${REGISTRY}/aws-k8s-tester:${TAG}

mkdir -p ./bin/

CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) \
  go build -v \
  -ldflags "-s -w \
  -X github.com/aws/aws-k8s-tester/version.GitCommit=${GIT_COMMIT} \
  -X github.com/aws/aws-k8s-tester/version.ReleaseVersion=${RELEASE_VERSION} \
  -X github.com/aws/aws-k8s-tester/version.BuildTime=${BUILD_TIME}" \
  -o ./bin/aws-k8s-tester \
  ./cmd/aws-k8s-tester

docker build \
  --tag ${REGISTRY}/aws-k8s-tester:${TAG} \
  --file ./Dockerfile .

docker push ${REGISTRY}/aws-k8s-tester:${TAG}
