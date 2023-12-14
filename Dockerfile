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

# alpine base for buildkit image
# TODO: remove this when alpine image supports riscv64
#FROM alpine:${ALPINE_VERSION} AS alpine-amd64
#FROM alpine:${ALPINE_VERSION} AS alpine-arm
#FROM alpine:${ALPINE_VERSION} AS alpine-arm64
#FROM alpine:${ALPINE_VERSION} AS alpine-s390x
#FROM alpine:${ALPINE_VERSION} AS alpine-ppc64le
#FROM alpine:edge@sha256:2d01a16bab53a8405876cec4c27235d47455a7b72b75334c614f2fb0968b3f90 AS alpine-riscv64
#FROM yangzewei2023/alpine:v3.18-base AS alpine-loong64
#FROM alpine-$TARGETARCH AS alpinebase
#FROM yangzewei2023/debian:sid AS debian-loong64
FROM lcr.loongnix.cn/library/debian:sid AS debianbase

# xx is a helper for cross-compilation
FROM lcr.loongnix.cn/tonistiigi/xx:1.3.0 AS xx

# go base image
FROM lcr.loongnix.cn/library/golang:1.21-buster AS golatest
ENV GOPROXY=https://goproxy.cn

# git stage is used for checking out remote repository sources
FROM lcr.loongnix.cn/library/debian:sid AS git
RUN apt update && apt install -y git

# gobuild is base stage for compiling go/cgo
FROM golatest AS gobuild-base
#RUN apk add --no-cache file bash clang musl-dev pkgconfig git make
RUN apt update && apt install -y file bash clang libc6-dev pkg-config git make
COPY --from=xx / /

# runc source
FROM git AS runc-src
ARG RUNC_VERSION
WORKDIR /usr/src

# build runc binary
FROM gobuild-base AS runc
#RUN git clone -b loong64-1.1.7 --depth 1 https://github.com/Loongson-Cloud-Community/runc.git $GOPATH/src/github.com/opencontainers/runc
#RUN unset https_proxy
WORKDIR $GOPATH/src/github.com/opencontainers/runc
RUN export GOPROXY=https://goproxy.cn
ARG TARGETPLATFORM
RUN set -e; apt update && apt install -y libc6-dev gcc libseccomp-dev libseccomp2
#RUN make static && mv runc /usr/bin
RUN wget http://cloud.loongnix.cn/releases/loongarch64/docker/runc/1.1.12/runc-seccomp-1.1.12-abi2.0-bin.tar.gz && \
tar xf runc-seccomp-1.1.12-abi2.0-bin.tar.gz && \
mv runc-seccomp-1.1.12-abi2.0-bin/runc-static /usr/bin/runc && rm -rf runc-seccomp-1.1.12-abi2.0-bin*
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
RUN mkdir /src
RUN wget https://github.com/Loongson-Cloud-Community/buildkit/releases/download/0.12.3/buildkit-0.12.3.tar.gz -O /src/buildkit.tar.gz && cd /src && tar xf buildkit.tar.gz
#COPY buildkit-0.12.3.tar.gz /src
WORKDIR /src
#RUN tar -zxvf buildkit.tar.gz
COPY .ldflags /tmp/.ldflags
RUN go build -ldflags "$(cat /tmp/.ldflags) -extldflags '-static'" -tags "osusergo netgo static_build seccomp ${BUILDKITD_TAGS}" -o /usr/bin/buildkitd ./cmd/buildkitd \
    && go build -ldflags "$(cat /tmp/.ldflags)" -o /usr/bin/buildctl ./cmd/buildctl \
    && rm -rf /src/*

#FROM lcr.loongnix.cn/library/tonistiigi/binfmt:latest AS binfmt-base
FROM lcr.loongnix.cn/tonistiigi/binfmt:qemu-8.0.5 AS binfmt-base

FROM scratch AS binfmt
COPY --from=binfmt-base /usr/bin/buildkit-qemu-* /

FROM debianbase AS buildkit-export
RUN apt update && apt install -y fuse3 git openssh-server pigz xz-utils 
  #&& ln -s fusermount3 /usr/bin/fusermount
COPY examples/buildctl-daemonless/buildctl-daemonless.sh /usr/bin/
VOLUME /var/lib/buildkit

FROM gobuild-base AS containerd
ARG CONTAINERD_VERSION
ARG CONTAINERD_ALT_VERSION
WORKDIR /usr/src
#RUN  git clone -b loong64-v1.7.2 --depth 1 https://github.com/Loongson-Cloud-Community/containerd.git containerd && unset https_proxy
#RUN cd containerd \
#    && make bin/containerd \
#    && make bin/containerd-shim-runc-v2 \
#    && make bin/ctr \
#    && mv bin /out
#RUN cd containerd \
#    && make STATIC=1 binaries \
#    && mv bin /out
RUN wget http://cloud.loongnix.cn/releases/loongarch64/docker/containerd/1.7.13/containerd-1.7.13-static-abi2.0-bin.tar.gz && \
tar xf containerd-1.7.13-static-abi2.0-bin.tar.gz && mv containerd-1.7.13-static-abi2.0-bin bin && mv bin /out && \
rm -rf containerd-1.7.13-static-abi2.0-bin*

FROM git AS binaries-linux
RUN apt update && apt install -y libc6-dev gcc libseccomp-dev libseccomp2
COPY --from=runc /usr/bin/runc /buildkit-runc
COPY --from=binfmt / /
COPY --from=buildkit /usr/bin/buildctl /
COPY --from=buildkit /usr/bin/buildkitd /
COPY --from=containerd /out/containerd* /

FROM binaries-linux AS binaries
# enable scanning for this stage
ARG BUILDKIT_SBOM_SCAN_STAGE=true

# containerd v1.6 for integration tests
FROM containerd as containerd-alt-16
WORKDIR /usr/src/tmp
ARG CONTAINERD_ALT_VERSION_16
RUN apt update && apt install -y btrfs-progs libbtrfsutil1 libbtrfs-dev libbtrfsutil-dev  \
    && rm -rf /usr/src/containerd  \
#    && git clone -b loong64-v1.6.21 --depth 1 https://github.com/Loongson-Cloud-Community/containerd.git containerd \
#    && cd containerd && echo "$(ls -la bin)" && unset https_proxy \
#    && make STATIC=1 binaries \
#    && make bin/containerd \
#    && make bin/containerd-shim-runc-v2 \
#    && make bin/ctr \
    && wget http://cloud.loongnix.cn/releases/loongarch64/docker/containerd/1.7.13/containerd-1.7.13-static-abi2.0-bin.tar.gz && tar xf containerd-1.7.13-static-abi2.0-bin.tar.gz && mv containerd-1.7.13-static-abi2.0-bin bin && \
    rm -rf containerd-1.7.13-static-abi2.0-bin* \
    && mkdir -p /usr/alt-16  mv bin /usr/alt-16 \
    && rm -rf /usr/src/tmp

#ARG REGISTRY_VERSION
#FROM yangzewei2023/distribution-registry:2.7.1-debian AS registry

FROM gobuild-base AS rootlesskit
ARG ROOTLESSKIT_VERSION
RUN git clone -b loong64-v1.0.1 https://github.com/Loongson-Cloud-Community/rootlesskit.git /go/src/github.com/rootless-containers/rootlesskit && unset https_proxy
WORKDIR /go/src/github.com/rootless-containers/rootlesskit
ARG TARGETPLATFORM
RUN go build -o /rootlesskit ./cmd/rootlesskit && rm -rf /go/src/github.com/rootless-containers/rootlesskit

FROM buildkit-export AS buildkit-linux
#COPY --link --from=binaries / /usr/bin/
RUN apt update && apt install -y libseccomp-dev libseccomp2
COPY --from=binaries / /usr/bin/
ENV BUILDKIT_SETUP_CGROUPV2_ROOT=1
ENTRYPOINT ["buildkitd"]


## buildkit builds the buildkit container image
#FROM buildkit-linux AS buildkit
#RUN apt update && apt install -y libseccomp-dev libseccomp2
#COPY --from=binaries / /usr/bin/
#ENV BUILDKIT_SETUP_CGROUPV2_ROOT=1
#ENTRYPOINT ["buildkitd"]
