# MosDNS v4.5.3. Koyeb builds linux/amd64 for this deployment.
# The previous digest was accidentally truncated; use the verified v4.5.3 tag.
FROM --platform=linux/amd64 irinesistiana/mosdns:v4.5.3

WORKDIR /etc/mosdns

COPY content/config.yaml ./config.yaml
COPY content/entrypoint.sh ./entrypoint.sh

RUN chmod 0755 ./entrypoint.sh

ENV PORT=8080
ENV DOH_PATH=/dns-query

EXPOSE 8080

ENTRYPOINT ["/etc/mosdns/entrypoint.sh"]
