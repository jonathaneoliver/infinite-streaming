#!/bin/sh
#
# launch.sh [<server-num>]
#
# The parameter (server-num) determines the base port for nginx.
# Server number 1 is on port 20080.  Server number 2 is on port 20180.
# Server number 3 is on port 20280.  Etc.


server=$(( (($1+0)<1)?1:($1+0) ))
serverbaseport=$(( (200 + server-1)*100 + 80 ))

# Generate nginx config from template with environment variable substitution
envsubst '${BOSS_OUTPUT_DIR}' < /etc/nginx/http.d/boss-content.conf.template > /etc/nginx/http.d/boss-content.conf

# Start background processes and nginx
# All processes now log to stdout/stderr for proper Docker log interleaving
( echo "Go mode." ) && \
( /usr/local/bin/go-upload & ) && \
( /usr/local/bin/go-live & ) && \
( echo "Go upload service handles /api/*;" ) && \
update-nginx-config.sh $serverbaseport && \
nginx -g 'daemon off;'
