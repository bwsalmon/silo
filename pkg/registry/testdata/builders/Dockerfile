FROM alpine:3.20
RUN adduser -D -u 1234 builder \
 && echo owned > /owned && chown 1234:1234 /owned \
 && rm /etc/motd \
 && ls -ln /owned
USER 1234
