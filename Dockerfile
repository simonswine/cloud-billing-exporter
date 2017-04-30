FROM alpine:3.5

# install ca certificates for comms with Let's Encrypt
RUN apk --update add ca-certificates && rm -rf /var/cache/apk/*

# add user / group
RUN addgroup -g 1000 app && \
    adduser -G app -h /home/app -u 1000 -D app

# move to user / group
USER app
WORKDIR /home/app

EXPOSE 9660

COPY _build/gcp-billing-exporter-linux-amd64 /gcp-billing-exporter
ENTRYPOINT ["/gcp-billing-exporter"]
ARG VCS_REF
LABEL org.label-schema.vcs-ref=$VCS_REF \
      org.label-schema.vcs-url="https://github.com/simonswine/gcp-billing-exporter" \
      org.label-schema.license="Apache-2.0"
