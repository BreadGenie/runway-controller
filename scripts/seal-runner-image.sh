#!/usr/bin/env bash
# Run only inside a fresh, never-registered image-builder guest, after installation.
set -euo pipefail
set +x
[[ $# == 1 && $1 == --inside-unregistered-image-builder && $EUID == 0 ]] || {
  echo 'Run as root inside an unregistered builder guest with --inside-unregistered-image-builder.' >&2
  exit 2
}
runner=/home/runner/actions-runner
[[ -f /etc/runway-runner-image.txt && -x $runner/bin/Runner.Listener ]] || {
  echo 'The guest runner installer must finish before sealing.' >&2
  exit 1
}
for credential in .runner .credentials .credentials_rsaparams; do
  [[ ! -e $runner/$credential ]] || {
    echo 'Refusing to capture a registered runner. Start from a new guest.' >&2
    exit 1
  }
done
[[ -z $(find "$runner" -maxdepth 1 -name '.runner*' -print -quit) ]] || exit 1
systemctl stop docker.service docker.socket containerd.service atlas-metadata.service
# The clean image has no jobs or containers. Each boot gets fresh daemon state.
rm -rf /var/lib/docker /var/lib/containerd "$runner/_diag" "$runner/_work"
apt-get clean
rm -rf /var/lib/apt/lists/*
cloud-init clean --logs --machine-id
find /etc/ssh -maxdepth 1 -type f -name 'ssh_host_*' -delete
for home in /root /home/runner /home/ubuntu; do
  [[ -d $home ]] || continue
  rm -rf "$home/.ssh" "$home/.cache"
  rm -f "$home/.bash_history" "$home/.python_history" "$home/.git-credentials"
done
rm -f /var/lib/systemd/random-seed /var/lib/dbus/machine-id
ln -s /etc/machine-id /var/lib/dbus/machine-id
printf 'runway-runner\n' > /etc/hostname
printf '127.0.0.1 localhost\n::1 localhost ip6-localhost ip6-loopback\n' > /etc/hosts
find /var/log -type f -exec truncate -s 0 {} +
rm -rf /tmp/* /var/tmp/*
sync
printf 'Sealed unregistered image; stop this VM, capture, then destroy it. Do not register or reuse it.\n'
