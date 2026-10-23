# Build only with explicitly staged, hash-pinned artifacts. No payload or guest
# image is fetched by this Dockerfile or by the runtime helper.
ARG RUNNER_BASE
FROM ${RUNNER_BASE}
RUN apt-get update && apt-get install -y --no-install-recommends \
    qemu-system-x86 qemu-utils ovmf genisoimage socat openssh-client \
    python3 nftables ca-certificates && rm -rf /var/lib/apt/lists/*
COPY artifacts/base.qcow2 /opt/sub2/base.qcow2
COPY artifacts/template.json /opt/sub2/template.json
COPY artifacts/payload/ /opt/sub2/payload/
COPY runner probe firewall qmp guest-command apply observe /usr/local/lib/sub2-kvm/
COPY guest/ /usr/local/lib/sub2-kvm/guest/
RUN chmod 0755 /usr/local/lib/sub2-kvm/runner /usr/local/lib/sub2-kvm/probe \
    /usr/local/lib/sub2-kvm/firewall /usr/local/lib/sub2-kvm/qmp \
    /usr/local/lib/sub2-kvm/guest-command /usr/local/lib/sub2-kvm/apply \
    /usr/local/lib/sub2-kvm/observe \
    && mkdir -p /slot/home /slot/run /slot/kvm /slot/credentials
ENTRYPOINT ["/usr/local/lib/sub2-kvm/runner"]
