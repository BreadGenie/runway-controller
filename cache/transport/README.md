# Cache transport

Bounded cache-v1 routing from the guest relay to the authenticated TLS data service.
Only allowed cache paths cross this boundary. The host injects the signed grant;
download ticket reads carry no bearer. Downloads stream with bounded concurrency,
size and deadlines. See the [spec](../../specs/runway-regional-cache.md).
