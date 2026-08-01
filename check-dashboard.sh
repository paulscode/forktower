#!/bin/sh
#
# Is the dashboard answering?
#
# The blocking signal, and deliberately not dependent on the second Bitcoin node:
# a chain still syncing is the ordinary state of a fresh install, and a package
# that will not come up until it finishes is one the user cannot look at while
# they wait — which is exactly when they most want to.
set -eu

if curl -fsS --max-time 10 -o /dev/null "http://127.0.0.1:8330/api/v1/healthz"; then
  echo '{"version":0,"message":"The dashboard is ready.","result":"success"}'
  exit 0
fi

echo '{"version":0,"message":"The dashboard is starting.","result":"starting"}'
exit 0
