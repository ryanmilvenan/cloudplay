ARG BUILD_PATH=/tmp/cloud-game
ARG VERSION=master

# base build stage
FROM ubuntu:plucky AS build0
ARG GO=1.26.0
ARG GO_DIST=go${GO}.linux-amd64.tar.gz

ADD https://go.dev/dl/$GO_DIST ./
RUN tar -C /usr/local -xzf $GO_DIST && \
    rm $GO_DIST
ENV PATH="${PATH}:/usr/local/go/bin"

RUN apt-get -q update && apt-get -q install --no-install-recommends -y \
    ca-certificates \
    make \
    upx \
&& rm -rf /var/lib/apt/lists/*

# next conditional build stage
FROM build0 AS build_coordinator
ARG BUILD_PATH
ARG VERSION
ENV GIT_VERSION=${VERSION}

WORKDIR ${BUILD_PATH}

# by default we ignore all except some folders and files, see .dockerignore
COPY . ./
RUN make build.coordinator
RUN find ./bin/* | xargs upx --best --lzma

WORKDIR /usr/local/share/cloud-game
RUN mv ${BUILD_PATH}/bin/* ./ && \
    mv ${BUILD_PATH}/LICENSE ./
RUN ${BUILD_PATH}/scripts/mkdirs.sh

# next worker build stage
FROM build0 AS build_worker
ARG BUILD_PATH
ARG VERSION
ENV GIT_VERSION=${VERSION}

WORKDIR ${BUILD_PATH}

# install deps
RUN apt-get -q update && apt-get -q install --no-install-recommends -y \
    build-essential \
    libopus-dev \
    libsdl2-dev \
    libvpx-dev \
    libyuv-dev \
    libjpeg-turbo8-dev \
    libx264-dev \
    libspeexdsp-dev \
    libavcodec-dev \
    libavutil-dev \
    libvulkan-dev \
    libegl-dev \
    libx11-dev \
    nvidia-cuda-toolkit \
    pkg-config \
&& rm -rf /var/lib/apt/lists/*

# by default we ignore all except some folders and files, see .dockerignore
COPY . ./

# Build the vendored rcheevos static library for CGo linkage. rcheevos is
# headers + 33 .c files (we skip rc_libretro.c — it depends on libretro.h
# which we don't need for the client-side evaluator).
RUN cd pkg/worker/rcheevos/upstream && \
    mkdir -p build && cd build && \
    gcc -c -O2 -fPIC -I../include -I../src \
        $(find ../src -name "*.c" -not -name "rc_libretro.c") && \
    ar rcs librcheevos.a *.o && \
    rm -f *.o

RUN make GO_TAGS=static,st,vulkan,nvenc build.worker
# UPX disabled for worker — causes CGo unsafe.Pointer corruption with Go 1.21+
# RUN find ./bin/* | xargs upx --best --lzma

WORKDIR /usr/local/share/cloud-game
RUN mv ${BUILD_PATH}/bin/* ./ && \
    mv ${BUILD_PATH}/LICENSE ./
RUN ${BUILD_PATH}/scripts/mkdirs.sh worker

FROM scratch AS coordinator

COPY --from=build_coordinator /usr/local/share/cloud-game /cloud-game
# autocertbot (SSL) requires these on the first run
COPY --from=build_coordinator /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

FROM ubuntu:plucky AS worker

RUN apt-get -q update && apt-get -q install --no-install-recommends -y \
    curl \
    ffmpeg \
    libcudart12 \
    libegl1 \
    libvulkan1 \
    libx11-6 \
    libxext6 \
 && apt-get autoremove \
 && rm -rf /var/lib/apt/lists/* /var/log/* /usr/share/bug /usr/share/doc /usr/share/doc-base \
    /usr/share/X11/locale/*

COPY --from=build_worker /usr/local/share/cloud-game /cloud-game
COPY --from=build_worker /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

ADD https://github.com/sergystepanov/mesa-llvmpipe/releases/download/v1.0.0/libGL.so.1.5.0 \
    /usr/lib/x86_64-linux-gnu/
RUN cd /usr/lib/x86_64-linux-gnu && \
    ln -sf libGL.so.1.5.0 libGL.so.1 && \
    ln -sf libGL.so.1 libGL.so

FROM worker AS cloud-game

WORKDIR /usr/local/share/cloud-game

COPY --from=coordinator /cloud-game ./
COPY --from=worker /cloud-game ./
