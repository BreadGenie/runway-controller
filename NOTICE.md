# Source and dependency attribution

Runway is licensed under GNU AGPL version 3. See [LICENSE](LICENSE).

The Atlas integration patch in `patches/atlas-lazy-route53-client.patch` includes context from [frappe/atlas](https://github.com/frappe/atlas) at commit `339735de16471e90232cf8b1ff840696ee830ad9`, licensed under GNU AGPL version 3. It changes Route53 client initialization and includes its focused tests and documentation. It is supplied for upstream review; it is not automatically applied.

The Go controller uses [actions/scaleset](https://github.com/actions/scaleset) at commit `e6daac702355cdb5b880b4fbdcf6d85dcd9e48e5`, licensed under MIT. Dependencies are resolved through Go modules; upstream source is not vendored.

The kernel build script downloads [Amazon Linux's Linux kernel source](https://github.com/amazonlinux/linux) at commit `8c3eeb0cc41e3fa4e5e27e29c557124ed1a1c451` and a [Firecracker guest configuration](https://github.com/firecracker-microvm/firecracker) at commit `791763f545dd73c1f5f22c67dd314009ad8047d9`. Linux is licensed under GPL version 2; Firecracker is licensed under Apache version 2. No kernel binary or downloaded source archive is included in this repository.

The guest installer downloads the [GitHub Actions runner](https://github.com/actions/runner), licensed under MIT, and installs packages from Ubuntu repositories. Its archive version and checksum are recorded in the installer. Downloaded binaries and package data are excluded from the repository.
