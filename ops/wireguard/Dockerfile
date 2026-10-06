FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive OCPOOL_VPN_BACKEND=wireguard
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl iproute2 iptables iputils-ping wireguard-tools \
    && rm -rf /var/lib/apt/lists/*
COPY ocpool-egress /usr/local/bin/ocpool-egress
COPY entrypoint.sh /usr/local/bin/ocpool-wireguard-entrypoint
RUN chmod 0755 /usr/local/bin/ocpool-egress /usr/local/bin/ocpool-wireguard-entrypoint
EXPOSE 18071
ENTRYPOINT ["/usr/local/bin/ocpool-wireguard-entrypoint"]
