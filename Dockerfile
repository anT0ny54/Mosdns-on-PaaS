FROM irinesistiana/mosdns:v5.3.4

COPY content /etc/mosdns

ENV PORT=8080 \
    DOH_PATH=/dns-query

EXPOSE 8080

ENTRYPOINT ["/etc/mosdns/entrypoint.sh"]
