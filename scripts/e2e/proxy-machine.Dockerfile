# The front reverse proxy the real NAS has and the plain-HTTP rig did not.
#
# rclone-manager#730 is the Activity page's fetch(/api/v1/activity) throwing
# TypeError: Failed to fetch on a real 0.4.0 deployment, while curl to the
# same route answers cleanly. The client request is byte-identical to every
# other page's (a relative, same-origin GET through the same api client), so
# a same-origin relative fetch cannot be failing on CORS or mixed content.
# What the browser has on the real NAS and the plain-HTTP rig does not is a
# front reverse proxy terminating TLS and speaking HTTP/2 to the browser:
# browsers only negotiate h2 over TLS, so the plain-HTTP rig drives the whole
# stack over HTTP/1.1 and never exercises the transport the operator's does.
#
# This container is that missing hop, added in front of `rbm-web serve-ui`:
#
#     browser --TLS/h2--> THIS nginx --http/1.1--> serve-ui --> serve
#
# It is a stock nginx reverse proxy, deliberately ordinary: an operator's
# front door, not a special one built to trip the bug. If a real, ordinary
# h2 front proxy in front of the real 0.4.0 image reproduces the throw, the
# fault is in what serve-ui/the engine put on the wire for that route; if it
# does not, the trigger is something more specific to the operator's own
# front end, and this rig has narrowed it either way.
FROM nginx:1.27-alpine

# openssl for the self-signed leaf; the client trusts it with
# ignoreHTTPSErrors, because this rig is exercising the h2/transport path,
# not certificate trust.
RUN apk add --no-cache openssl \
 && mkdir -p /etc/nginx/tls \
 && openssl req -x509 -newkey rsa:2048 -nodes -days 825 \
      -keyout /etc/nginx/tls/key.pem \
      -out /etc/nginx/tls/cert.pem \
      -subj "/CN=rclone-manager" \
      -addext "subjectAltName=DNS:rclone-manager,DNS:localhost" \
 && chmod 0644 /etc/nginx/tls/key.pem

# The upstream is reached by the edge-network alias the rig gives serve-ui
# when the front proxy is in play (see three-machine-web-ui.sh).
COPY proxy.nginx.conf /etc/nginx/nginx.conf

# Fail the build if the config does not parse, rather than at container
# start where the rig would only see a proxy that never came up.
RUN nginx -t -c /etc/nginx/nginx.conf

EXPOSE 443
