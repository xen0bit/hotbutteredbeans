# The full build (model and ONNX Runtime inside) on a small glibc base, with git for
# `hbb diff`. goreleaser (dockers_v2) puts each platform's binary at $TARGETPLATFORM/hbb.
#
#   docker run --rm -v "$PWD:/src" ghcr.io/xen0bit/hbb diff origin/main...HEAD --compare
FROM debian:trixie-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 # repositories mounted from the host belong to another user
 && git config --system --add safe.directory '*'

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/hbb /usr/local/bin/hbb
COPY licenses /usr/share/doc/hbb/licenses

# A cache any user can write (CI runners use many uids); ONNX Runtime is unpacked into it
# now, so a container never needs to write or download anything to start.
ENV HBB_CACHE_DIR=/var/cache/hbb
RUN mkdir -p /var/cache/hbb && hbb runtime path && chmod -R a+rwX /var/cache/hbb

WORKDIR /src
ENTRYPOINT ["hbb"]
CMD ["--help"]
