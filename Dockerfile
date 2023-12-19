# syntax=cr.loongnix.cn/library/dockerfile:experiment
ARG RUNC_VERSION=v1.1.7
ARG CONTAINERD_VERSION=v1.7.2
# containerd v1.6 for integration tests
ARG CONTAINERD_ALT_VERSION_16=v1.6.21
ARG REGISTRY_VERSION=2.8.0
ARG ROOTLESSKIT_VERSION=v1.0.1
ARG CNI_VERSION=v1.2.0
ARG STARGZ_SNAPSHOTTER_VERSION=v0.14.3
ARG NERDCTL_VERSION=v1.4.0
ARG DNSNAME_VERSION=v1.3.1
ARG NYDUS_VERSION=v2.1.6
ARG MINIO_VERSION=RELEASE.2022-05-03T20-36-08Z
ARG MINIO_MC_VERSION=RELEASE.2022-05-04T06-07-55Z
ARG AZURITE_VERSION=3.18.0
ARG GOTESTSUM_VERSION=v1.9.0

ARG GO_VERSION=1.20
ARG ALPINE_VERSION=3.18
ARG XX_VERSION=1.3.0

# minio for s3 integration tests
#FROM minio/minio:${MINIO_VERSION} AS minio
#FROM minio/mc:${MINIO_MC_VERSION} AS minio-mc

FROM cr.loongnix.cn/library/debian:buster AS debianbase

# xx is a helper for cross-compilation
FROM cr.loongnix.cn/tonistiigi/xx:1.3.0 AS xx

# go base image
FROM cr.loongnix.cn/library/golang:1.20-buster AS golatest

# git stage is used for checking out remote repository sources
FROM cr.loongnix.cn/library/debian:buster AS git
RUN apt update && apt install -y git

# gobuild is base stage for compiling go/cgo
FROM golatest AS gobuild-base
RUN apt update && apt install -y file bash clang libc6-dev pkg-config git make
COPY --from=xx / /

# runc source
FROM git AS runc-src
ARG RUNC_VERSION
WORKDIR /usr/src

# build runc binary
FROM gobuild-base AS runc
RUN git clone -b loong64-1.1.7 --depth 1 https://github.com/Loongson-Cloud-Community/runc.git $GOPATH/src/github.com/opencontainers/runc
WORKDIR $GOPATH/src/github.com/opencontainers/runc
ARG TARGETPLATFORM
RUN set -e; apt update && apt install -y libc6-dev gcc libseccomp-dev libseccomp2
RUN make static && mv runc /usr/bin
RUN rm -rf $GOPATH/src/github.com/opencontainers/runc

FROM gobuild-base AS buildkit-base
ENV GOFLAGS=-mod=vendor

# scan the version/revision info
FROM buildkit-base AS buildkit-version
# TODO: PKG should be inferred from go modules

# build buildctl binary
FROM buildkit-base AS buildkit
ENV CGO_ENABLED=0
ARG TARGETPLATFORM
WORKDIR /src
RUN wget https://github.com/Loongson-Cloud-Community/buildkit/releases/download/0.12.3/buildkit-0.12.3.tar.gz -O /src/buildkit-0.12.3.tar.gz \
    && tar -zxvf buildkit-0.12.3.tar.gz
COPY .ldflags /tmp/.ldflags
RUN  go build -ldflags "$(cat /tmp/.ldflags) -extldflags '-static'" -tags "osusergo netgo static_build seccomp ${BUILDKITD_TAGS}" -o /usr/bin/buildkitd ./cmd/buildkitd \
    && go build -ldflags "$(cat /tmp/.ldflags)" -o /usr/bin/buildctl ./cmd/buildctl \
    && rm -rf /src/* \

FROM cr.loongnix.cn/tonistiigi/binfmt:latest AS binfmt-base
FROM scratch AS binfmt
COPY --link --from=cr.loongnix.cn/tonistiigi/binfmt:latest /usr/bin/*qemu* /

FROM debianbase AS buildkit-export
RUN apt update && apt install -y fuse3 git openssh-server pigz xz-utils 
COPY examples/buildctl-daemonless/buildctl-daemonless.sh /usr/bin/
VOLUME /var/lib/buildkit

FROM gobuild-base AS containerd
ARG CONTAINERD_VERSION
ARG CONTAINERD_ALT_VERSION
WORKDIR /usr/src
RUN  git clone -b loong64-v1.7.2 --depth 1 https://github.com/Loongson-Cloud-Community/containerd.git containerd 
RUN cd containerd \
    && make STATIC=1 binaries \
    && mv bin /out

FROM git AS binaries-linux
RUN apt update && apt install -y libc6-dev gcc libseccomp-dev libseccomp2
COPY --link --from=runc /usr/bin/runc /buildkit-runc
COPY --link --from=binfmt / /
COPY --link --from=buildkit /usr/bin/buildctl /
COPY --link --from=buildkit /usr/bin/buildkitd /
COPY --link --from=containerd /out/containerd* /

FROM binaries-linux AS binaries
# enable scanning for this stage
ARG BUILDKIT_SBOM_SCAN_STAGE=true

# containerd v1.6 for integration tests
FROM containerd as containerd-alt-16
WORKDIR /usr/src/tmp
ARG CONTAINERD_ALT_VERSION_16
RUN apt update && apt install -y btrfs-progs libbtrfsutil1 libbtrfs-dev libbtrfsutil-dev  \
    && rm -rf /usr/src/containerd  \
    && git clone -b loong64-v1.6.21 --depth 1 https://github.com/Loongson-Cloud-Community/containerd.git containerd \
    && cd containerd && echo "$(ls -la bin)"  \
    && make STATIC=1 binaries \
    && mkdir -p /usr/alt-16  mv bin /usr/alt-16 \
    && rm -rf /usr/src/tmp


FROM gobuild-base AS rootlesskit
ARG ROOTLESSKIT_VERSION
RUN git clone -b loong64-v1.0.1 https://github.com/Loongson-Cloud-Community/rootlesskit.git /go/src/github.com/rootless-containers/rootlesskit 
WORKDIR /go/src/github.com/rootless-containers/rootlesskit
ARG TARGETPLATFORM
RUN go build -o /rootlesskit ./cmd/rootlesskit && rm -rf /go/src/github.com/rootless-containers/rootlesskit

FROM buildkit-export AS buildkit-linux
COPY --link --from=binaries / /usr/bin/
ENV BUILDKIT_SETUP_CGROUPV2_ROOT=1
ENTRYPOINT ["buildkitd"]

