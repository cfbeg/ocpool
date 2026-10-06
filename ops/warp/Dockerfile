FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl gnupg dbus iproute2 iptables procps \
    && curl -fsSL https://pkg.cloudflareclient.com/pubkey.gpg | gpg --yes --dearmor --output /usr/share/keyrings/cloudflare-warp-archive-keyring.gpg \
    && printf '%s\n' 'deb [signed-by=/usr/share/keyrings/cloudflare-warp-archive-keyring.gpg] https://pkg.cloudflareclient.com/ noble main' > /etc/apt/sources.list.d/cloudflare-client.list \
    && apt-get update && apt-get install -y --no-install-recommends cloudflare-warp \
    && rm -rf /var/lib/apt/lists/*

COPY ocpool-egress /usr/local/bin/ocpool-egress
COPY entrypoint.sh /usr/local/bin/ocpool-warp-entrypoint
RUN chmod 0755 /usr/local/bin/ocpool-egress /usr/local/bin/ocpool-warp-entrypoint
VOLUME /var/lib/cloudflare-warp
EXPOSE 18071
ENTRYPOINT ["/usr/local/bin/ocpool-warp-entrypoint"]
