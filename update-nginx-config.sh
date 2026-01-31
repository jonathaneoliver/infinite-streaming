#!/bin/sh

serverbaseport="${1:-20080}"

echo "###"
echo "### Updating BOSS server base port config to ${serverbaseport}..."
echo "###"

file='/etc/nginx/http.d/boss-content.conf'
cp "$file" "/tmp/${file##*/}"
sed -e "s/{SERVER_BASE_PORT}/${serverbaseport%80}/g" "/tmp/${file##*/}" > "$file"
rm -f "/tmp/${file##*/}"
