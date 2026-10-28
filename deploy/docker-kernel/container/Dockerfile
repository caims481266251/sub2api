# Build for the Docker host architecture: network tools must run natively.
# The pinned amd64 model payload uses host binfmt support on ARM64.
FROM debian@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad
RUN if [ "$(dpkg --print-architecture)" != amd64 ]; then dpkg --add-architecture amd64; fi \
 && apt-get update && apt-get install -y --no-install-recommends ca-certificates python3 util-linux nftables libc6:amd64 libgcc-s1:amd64 \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /slot/home /slot/run /slot/credentials /opt/kin-payload /usr/local/lib/sub2-container
COPY container/entry container/observe container/common.py container/proxy_bridge.py /usr/local/lib/sub2-container/
COPY container/firewall /usr/local/lib/sub2-container/firewall
RUN chmod 755 /usr/local/lib/sub2-container/entry /usr/local/lib/sub2-container/observe /usr/local/lib/sub2-container/firewall
LABEL com.sub2api.runtime="container-v1"
ENTRYPOINT ["/usr/local/lib/sub2-container/entry"]
