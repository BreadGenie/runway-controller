# Cache grants

Host-authorized repository/ref capabilities, signed with Ed25519 and bounded by
the verified job deadline and a thirty-minute maximum. Verification requires the
exact HTTPS audience, region and a narrow synchronized host clock interval.
The strict canonical wire format has no algorithm negotiation.

Setup derives scope from host-verified GitHub context. The host retains the
signing key and injects the bearer into the TLS data path; guests never receive
storage credentials. See the [standalone spec](../../specs/runway-regional-cache.md).
