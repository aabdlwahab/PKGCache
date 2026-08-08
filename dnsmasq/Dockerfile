FROM alpine:3.22

RUN apk add --no-cache dnsmasq

COPY dnsmasq.conf /etc/dnsmasq.conf
COPY entrypoint.sh /usr/local/bin/pkgreg-dns
RUN chmod 0755 /usr/local/bin/pkgreg-dns

EXPOSE 53/tcp 53/udp

ENTRYPOINT ["/usr/local/bin/pkgreg-dns"]
