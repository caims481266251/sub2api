# Native runner tools execute on the Docker host architecture. The guest disk,
# firmware and inference payload remain x86_64 on both amd64 and arm64 runners.
# Build with --platform matching the Docker execution host, not the guest.
ARG TOOLCHAIN_BASE=debian@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad
FROM ${TOOLCHAIN_BASE} AS tools
LABEL org.opencontainers.image.source="https://github.com/ranxi2001/sub2api" \
      com.sub2api.vm.runtime="upstream-native-runner"
ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates genisoimage openssh-client \
      ovmf=2025.02-8+deb13u1 python3 \
      qemu-system-x86=1:10.0.13+ds-0+deb13u1 \
      qemu-utils=1:10.0.13+ds-0+deb13u1 socat util-linux nftables \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /opt/kin-kvm/guest /slot/home /slot/run /slot/kvm \
      /opt/kin-guest/usr/local/bin /opt/kin-guest/opt/kin

# Copy only the fixed amd64 guest data. Never execute or copy the source
# image's amd64 QEMU/tool binaries into an arm64 runner.
FROM --platform=linux/amd64 ghcr.io/dofastted/kin-kvm-ubuntu@sha256:2c12ba55e5df2c677972d235d88be5200e9dcb778fb27af7eeed48f7b93fdde7 AS guest_image
FROM tools AS runtime
COPY --from=guest_image /opt/kin-kvm/base.qcow2 /opt/kin-kvm/base.id /opt/kin-kvm/base.url /opt/kin-kvm/
COPY kin-kvm-entry kin-guest-exec /usr/local/bin/
COPY guest/ /opt/kin-kvm/guest/
COPY entry probe apply observe common.py firewall /usr/local/lib/sub2-vm/
COPY LICENSE.vm2api README.md source.json /usr/share/doc/sub2-vm-runner/
RUN chmod 755 /usr/local/lib/sub2-vm/entry /usr/local/lib/sub2-vm/probe /usr/local/lib/sub2-vm/apply /usr/local/lib/sub2-vm/observe /usr/local/lib/sub2-vm/firewall \
 && chmod 755 /usr/local/bin/kin-kvm-entry /usr/local/bin/kin-guest-exec \
      /opt/kin-kvm/guest/kin-guest-setup /opt/kin-kvm/guest/install-guest.sh \
      /opt/kin-kvm/guest/kin-kernel-start /opt/kin-kvm/guest/kin-kernel-relay.py \
 && chmod 644 /opt/kin-kvm/guest/*.service /opt/kin-kvm/guest/user-data
LABEL kin.vm.runtime=kvm
ENTRYPOINT ["/usr/local/bin/kin-kvm-entry"]
