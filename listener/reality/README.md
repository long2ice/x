# Connection setup lifecycle

REALITY and PROXY-header parsing use bounded asynchronous setup. A slow peer
does not occupy the socket accept loop. On overload, excess new sockets are
closed immediately; established streams are not counted against setup limits.

- `reality.maxPending` (alias `maxPending`): defaults to 128 per REALITY listener,
  including authenticated connections waiting for the consumer to accept them.
- The PROXY wrapper also allows at most 128 pending connections per listener.
- Both stages share a process-wide limit of 1024 pending setups, so opening many
  ports does not multiply the memory bound without limit.
- `reality.handshakeTimeout` (alias `handshakeTimeout`): defaults to 15 seconds.
  Covers detection readiness, DNS/dial, client and dest I/O, failed-auth fallback,
  and waiting for delivery. Timeout or listener close cancels setup and closes
  both sockets. Deadlines/cancellation are removed when ownership is handed off.
- Optional PROXY detection retains plain TCP/server-first support after the
  header timeout. Malformed headers are rejected and prefetched bytes preserved.
- Dest probes are shared by dest/SNI/ALPN, have a 10-second total deadline, and
  use at most 32 sockets process-wide. All paths close their sockets. They are
  shared cache work, so can finish within that bound after an individual listener
  closes; they are not retained once finished.

Regression coverage includes slow and malformed PROXY v1/v2 clients, optional
plain TCP, overload/slot reuse, cancellation during dial and dest read/write,
unconsumed successful setups, concurrent close/delivery, and authenticated
REALITY sessions that remain usable after setup timeout and listener close.
