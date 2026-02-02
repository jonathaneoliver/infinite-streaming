#!/bin/sh

serverport="${1:-30000}"

echo "###"
echo "### Updating BOSS server listen port config to ${serverport}..."
echo "###"

file='/etc/nginx/http.d/boss-content.conf'
cp "$file" "/tmp/${file##*/}"
sed -e "s/{SERVER_PORT}/${serverport}/g" "/tmp/${file##*/}" > "$file"
rm -f "/tmp/${file##*/}"
