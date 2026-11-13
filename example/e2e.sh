#!/bin/sh
# E2E against a running example: hook forgery, pre-create auth, interrupted PATCH and resume, pre-finish retry.
# Usage: e2e.sh <file> <path to finished.log>
set -u
B=http://localhost:8080
F=$1
LOG=$2
H="-H Tus-Resumable:1.0.0"
clean() { grep -v -i "^Date\|^Server\|^X-Content-Type"; }
for i in $(seq 20); do curl -s -o /dev/null $B/ && break; perl -e 'select(undef,undef,undef,0.5)'; done
echo "== forged hook"; curl -s -o /dev/null -w '%{http_code}\n' -X POST -H 'Tus-Hook: pre-create' -H 'Content-Type: application/json' -d '{"Type":"pre-finish"}' $B/tus-hook.php
echo "== create without auth"; curl -s -i -X POST $H -H "Upload-Length: $(wc -c < $F | tr -d ' ')" $B/files/ | clean; echo
echo "== create"; LOC=$(curl -s -i -X POST -H 'Cookie: session=abc' -H 'Authorization: Bearer secret' $H -H "Upload-Length: $(wc -c < $F | tr -d ' ')" -H "Upload-Metadata: filename $(printf blob.bin | base64)" $B/files/ | tee /dev/stderr | grep -i '^Location' | tr -d '\r' | awk '{print $2}') 2>&1
echo "== interrupted PATCH"; curl -s -o /dev/null --limit-rate 300k -m 2 -X PATCH $H -H 'Upload-Offset: 0' -H 'Content-Type: application/offset+octet-stream' --data-binary @$F $LOC; echo "curl exit $?"
perl -e 'select(undef,undef,undef,0.5)'
echo "== HEAD"; OFF=$(curl -s -I $H $LOC | tee /dev/stderr | grep -i '^Upload-Offset' | tr -d '\r' | awk '{print $2}') 2>&1
echo "== resume from $OFF"; tail -c +$((OFF+1)) $F > $F.rest
curl -s -i -X PATCH -H 'Cookie: session=abc' $H -H "Upload-Offset: $OFF" -H 'Content-Type: application/offset+octet-stream' --data-binary @$F.rest $LOC | clean
echo; echo "local sha256: $(shasum -a 256 < $F)"
echo "== PHP log"; cat $LOG
echo "== GET (download disabled)"; curl -s -o /dev/null -w '%{http_code}\n' $H $LOC

echo "== base path without trailing slash"; curl -s -o /dev/null -w '%{http_code}\n' -X POST -H 'Authorization: Bearer secret' $H -H 'Upload-Length: 1' $B/files

echo "== pre-finish failure, then retry"
LOC=$(curl -s -i -X POST -H 'Authorization: Bearer secret' $H -H 'Upload-Length: 5' -H "Upload-Metadata: fail_finish" $B/files/ | grep -i '^Location' | tr -d '\r' | awk '{print $2}')
printf hello > $F.five
curl -s -i -X PATCH $H -H 'Upload-Offset: 0' -H 'Content-Type: application/offset+octet-stream' --data-binary @$F.five $LOC | head -1
curl -s -I $H $LOC | grep -i '^Upload-Offset'
curl -s -i -X PATCH $H -H 'Upload-Offset: 5' -H 'Content-Type: application/offset+octet-stream' --data-binary '' $LOC | head -1
