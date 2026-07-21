# gRPC transport feasibility gate — results

Date: 2026-07-20. Verdict: **PASS — gRPC-first is viable; proceed to the implementation
slice.** No blocking edge incompatibility found. One item (a clean middlebox idle-kill)
could not be forced by the synthetic proxy and is carried forward for measurement
against a real edge.

## Harness

- Server: throwaway control-plane test server built from the REAL generated C#
  contract (`GrpcServices="Server"`), reproducing the chosen topology — a dedicated
  TLS `Http1AndHttp2` gRPC endpoint (15443), a dedicated h2c `Http2`-only endpoint
  (15080, the behind-reverse-proxy upstream shape), and a separate HTTP/1.1 "REST"
  endpoint (15254) wearing HTTPS-redirect + JSON-exception middleware to prove the
  gRPC endpoints sit outside that pipeline.
- Client: Go `grpc-go` harness built from the SAME generated Go contract — so every
  scenario also proves cross-language Protobuf fidelity.
- Controllable HTTP CONNECT proxy (Go) with a drop-all control port and an optional
  idle timeout, simulating a corporate egress proxy.
- Two self-signed leaf certs (A, B) with recorded SPKI pins for the pinning and
  rotation scenarios.

Note: Go's proxy resolver always bypasses loopback targets, so proxy scenarios use a
routable LAN address as the target; SPKI pinning bypasses cert-name validation, so a
non-matching hostname is fine. The proxy log confirms the tunnel was genuinely in
path.

## Scenarios and results

| # | Scenario | Result |
| --- | --- | --- |
| 1 | Direct TLS (`Http1AndHttp2`, ALPN→h2), SPKI-pinned, 12 s session | PASS — register 544 ms, session accepted 48 ms, 13 assignments / 12 acks, RTT p50 1 ms / max 8 ms |
| 2 | h2c `Http2`-only (reverse-proxy-upstream topology), 8 s session | PASS — session accepted 11 ms, 8/7, RTT p50 1 ms |
| 3 | SPKI pin MISMATCH (negative) | PASS — handshake refused (`SPKI pin mismatch`), connection never established |
| 4 | Session THROUGH HTTP CONNECT proxy, TLS end-to-end | PASS — proxy tunnel logged to `…:15443`, HTTP/2 preserved, 6/5 delivered |
| 5 | Mid-stream interruption + reconnect (proxy drops tunnel at ~4 s) | PASS — failure detected (`Unavailable: EOF`), reconnect in 15 ms carrying the in-flight roster, delivery RESUMED |
| 6a | Idle 10 s, keepalive 60 s, through idle-timeout proxy | Stream survived (10 s idle < effective window in this harness) |
| 6b | Idle 10 s, keepalive 3 s + permit-without-stream | PASS — stream alive, probe RTT 2 ms |
| 7a | Cert rotation: server on cert B, pin set {A active, B backup} | PASS — backup pin matched; session established |
| 7b | Cert rotation without backup: server on cert B, pin set {A only} | PASS (negative) — REFUSED (`pin mismatch: peer B not in pin set`) — demonstrates a backup pin must be deployed BEFORE rotation |
| 8 | Control-plane restart mid-session | PASS — session-1 errored on kill (`Unavailable`, connection closed); a fresh session recovered cleanly after restart (accepted 44 ms, 6/5) |

## Findings carried forward

1. **HTTP/2 is preserved end-to-end** on the direct-TLS topology, the h2c upstream
   topology, and through a standard HTTP CONNECT proxy. gRPC cannot ride a downgraded
   hop, so a working bidi stream IS the preservation proof. An edge that terminates
   gRPC onto an HTTP/1.1 upstream does NOT support this as-is — the control plane
   must expose either the direct-TLS `Http1AndHttp2` endpoint or a reverse-proxy
   route with an h2c upstream. Both were proven here.
2. **Middleware isolation works**: the REST redirect/exception pipeline is scoped by
   local port and does not touch the gRPC endpoints. The production relay-auth scheme
   (metadata credential) is wired on the dedicated endpoint, outside the interactive
   user-auth pipeline.
3. **SPKI leaf pinning behaves as designed**, including the rotation lifecycle: a
   backup pin deployed ahead of rotation keeps the fleet connected; rotating without
   a pre-deployed backup pin bricks it. This validates the "≥1 backup pin at all
   times + refresh over the pinned stream" requirement.
4. **Reconnect is fast and lossless at the transport layer**: interruption and
   control-plane restart both recover in tens of milliseconds, and the in-flight
   roster rides the reconnect hello — the production reconciliation path.
5. **Keepalive is the idle-survival lever.** The synthetic proxy did not cleanly force
   an idle-kill (both idle cases survived 10 s), so the exact keepalive interval is
   NOT settled here — it is measured against a real customer edge, tuned below the
   observed middlebox idle timeout and above the server's keepalive-ping enforcement
   floor (the harness already carries the knobs).

No STOP condition triggered. gRPC-first stands.
