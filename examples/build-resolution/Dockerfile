FROM alpine:3.22

ARG CACHE_NAME=pkgcache.internal
ARG EXPECT_DNS=0

# The phase-3 contract: the build container itself resolves the cache name. This
# does not rely on the build host's /etc/hosts file.
RUN getent hosts "${CACHE_NAME}" \
 && if [ "${EXPECT_DNS}" = 1 ]; then nslookup "${CACHE_NAME}"; fi
