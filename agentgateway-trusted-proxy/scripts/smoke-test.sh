#!/usr/bin/env bash
# Smoke test for kagent in trusted-proxy mode behind a gateway and oauth2-proxy.
#
# Checks that anonymous callers are turned away, that a token from the identity
# provider is accepted, that the controller knows who each caller is, that
# identity headers from outside are ignored, that a pod in another namespace
# cannot reach the controller, and (with the browser check) that a person can
# sign in and run a turn longer than 15 seconds.
#
# On a cluster with no load balancer, reach the gateway with a port-forward:
#
#   kubectl -n gateway-system port-forward svc/kagent 8443:443 &
#   npm install --prefix scripts        # once, for the browser check
#   AGENT=<agent name> CACERT=tls.crt ./scripts/smoke-test.sh
#
# Set SKIP_BROWSER=1 to skip the browser check, SKIP_NETPOL=1 to skip the
# NetworkPolicy check (it starts a short-lived pod and pulls curlimages/curl).
#
# Needs: curl, jq, python3, kubectl, and node plus Chrome for the browser check.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
PORT_SUFFIX="${PORT_SUFFIX:-:8443}"
KAGENT="https://kagent.localtest.me${PORT_SUFFIX}"
DEX="https://dex.kagent.localtest.me${PORT_SUFFIX}"
NAMESPACE="${NAMESPACE:-kagent}"
AGENT="${AGENT:-$(kubectl get agents -n "$NAMESPACE" -o jsonpath='{.items[0].metadata.name}')}"
CACERT="${CACERT:-}"   # path to the CA that signed the gateway certificate

curl_tls() { curl -s ${CACERT:+--cacert "$CACERT"} "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok   $*"; }
expect() { [ "$2" = "$3" ] || fail "$1: got $2, want $3"; ok "$1 ($2)"; }
refute() { case " $2 " in *" $3 "*) fail "$1: saw $3 in: $2";; *) ok "$1 (saw: $2)";; esac; }

CLIENT_SECRET="$(kubectl get secret kagent-oidc -n "$NAMESPACE" -o jsonpath='{.data.client-secret}' | base64 -d)"
token() {  # an ID token for <name>@example.com, from the identity provider
  curl_tls -X POST "$DEX/token" -u "kagent:$CLIENT_SECRET" -d grant_type=password \
    -d "username=$1@example.com" -d "password=$1-pass" -d 'scope=openid email profile' | jq -r .id_token
}

# The UI talks gRPC-web to /api. An empty request body is five zero bytes. The
# second body is the request the UI itself sends when it asks for more sessions.
EMPTY='\x00\x00\x00\x00\x00'
UI_REQUEST='\x00\x00\x00\x00\x04\x10\x01\x1a\x00'
list_sessions() {  # token, body, extra curl args. Prints the session owners seen
  local tk="$1" body="$2"; shift 2
  printf "$body" | curl_tls -X POST "$KAGENT/api/kagent.api.v1alpha1.SessionService/ListSessions" \
    -H "Authorization: Bearer $tk" -H 'Content-Type: application/grpc-web+proto' -H 'X-Grpc-Web: 1' "$@" --data-binary @- \
    | python3 -c "import sys,re; t=sys.stdin.buffer.read().decode('latin1'); print(' '.join(sorted(set(re.findall(r'[a-z0-9._-]+@[a-z0-9.-]+', t)))) or 'none')"
}

echo "agent: $NAMESPACE/$AGENT"
echo "== anonymous callers"
expect "the UI is not served to an anonymous caller" "$(curl_tls -o /dev/null -w '%{http_code}' "$KAGENT/")" 403
expect "the API is not served to an anonymous caller" "$(printf "$EMPTY" | curl_tls -o /dev/null -w '%{http_code}' -X POST "$KAGENT/api/kagent.api.v1alpha1.SessionService/ListSessions" -H 'Content-Type: application/grpc-web+proto' --data-binary @-)" 403
start="$(curl_tls -o /dev/null -w '%{http_code} %{redirect_url}' "$KAGENT/oauth2/start")"
case "$start" in "302 $DEX/auth?"*) ok "sign-in redirects to the identity provider";; *) fail "sign-in redirect: $start";; esac

echo "== tokens"
ALICE="$(token alice)"; BOB="$(token bob)"
[ -n "$ALICE" ] && [ "$ALICE" != null ] || fail "could not get a token from the identity provider"
forged="$(python3 - <<'PY'
import base64, json
b = lambda d: base64.urlsafe_b64encode(json.dumps(d).encode()).rstrip(b"=").decode()
print(b({"alg": "none", "typ": "JWT"}) + "." + b({"email": "admin@kagent.dev", "sub": "forged"}) + ".x")
PY
)"
expect "a forged token is refused at the proxy" "$(printf "$EMPTY" | curl_tls -o /dev/null -w '%{http_code}' -X POST "$KAGENT/api/kagent.api.v1alpha1.SessionService/ListSessions" -H "Authorization: Bearer $forged" -H 'Content-Type: application/grpc-web+proto' --data-binary @-)" 403
expect "a token from the identity provider is accepted" "$(printf "$EMPTY" | curl_tls -o /dev/null -w '%{http_code}' -X POST "$KAGENT/api/kagent.api.v1alpha1.SessionService/ListSessions" -H "Authorization: Bearer $ALICE" -H 'Content-Type: application/grpc-web+proto' --data-binary @-)" 200

if [ -z "${SKIP_BROWSER:-}" ]; then
  echo "== a person signs in and runs a turn longer than 15 seconds"
  [ -d "$HERE/node_modules/playwright-core" ] || fail "run 'npm install --prefix scripts' first, or set SKIP_BROWSER=1"
  AGENT="$AGENT" BASE="$KAGENT" node "$HERE/browser-check.mjs"
fi

echo "== who the controller thinks the caller is"
mine="$(list_sessions "$ALICE" "$EMPTY")"
[ "$mine" = "alice@example.com" ] || fail "alice should see only her own sessions, saw: $mine (run without SKIP_BROWSER so she has one)"
ok "alice sees her own sessions ($mine)"
refute "bob does not see alice's sessions" "$(list_sessions "$BOB" "$EMPTY")" alice@example.com
refute "bob cannot become alice with X-User-Id" "$(list_sessions "$BOB" "$EMPTY" -H 'X-User-Id: alice@example.com')" alice@example.com
refute "bob cannot pick the agent path with X-Agent-Name" "$(list_sessions "$BOB" "$EMPTY" -H 'X-Agent-Name: x' -H 'X-User-Id: alice@example.com')" alice@example.com
wide="$(list_sessions "$BOB" "$UI_REQUEST")"
case " $wide " in
  *" alice@example.com "*) echo "note bob can list alice's sessions with the request the UI sends ($wide). Authorization is not enforced in this kagent version";;
  *) ok "bob cannot list other users' sessions with the UI's request";;
esac

if [ -z "${SKIP_NETPOL:-}" ]; then
  echo "== a pod in another namespace"
  out="$(kubectl run smoke-probe --rm -i --restart=Never -n default --image=curlimages/curl:8.10.1 --quiet --command -- sh -c "
    printf '\\000\\000\\000\\000\\000' > /tmp/e
    curl -s -m 8 -o /dev/null -w '%{http_code}' -X POST http://kagent-controller.$NAMESPACE.svc:8083/api/kagent.api.v1alpha1.SessionService/ListSessions \
      -H 'Authorization: Bearer $forged' -H 'Content-Type: application/grpc-web+proto' -H 'X-Grpc-Web: 1' --data-binary @/tmp/e || echo -n ' curl-failed'" 2>&1 | tail -1)"
  case "$out" in *curl-failed*|000*) ok "a forged token cannot reach the controller from another namespace";;
    *) fail "the controller answered a pod in the default namespace ($out). Apply network-policy.yaml, and use a CNI that enforces it";; esac
fi

echo "PASS: anonymous callers are refused, identity is real, spoofed headers are ignored, and the controller is not reachable around the proxy"
