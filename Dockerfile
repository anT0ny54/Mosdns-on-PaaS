FROM irinesistiana/mosdns@sha256:db2db6c7cdce

WORKDIR /etc/mosdns

COPY content/config.yaml ./config.yaml
COPY content/entrypoint.sh ./entrypoint.sh

RUN chmod 0755 ./entrypoint.sh

ENV PORT=8080
ENV DOH_PATH=/dns-query

EXPOSE 8080

ENTRYPOINT ["/etc/mosdns/entrypoint.sh"]
