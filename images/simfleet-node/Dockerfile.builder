# Build host for the simfleet-node containerDisk.
#
# The e2e runners cannot run libguestfs directly: Ubuntu ships /boot/vmlinuz-*
# as root-only and the runner user is not root, so supermin fails copying the
# kernel into its appliance. The runner user is in the docker
# group, so the recipe runs libguestfs in here instead, where it is root and
# the kernel it needs is the one this image installs.
FROM ubuntu:24.04

# isc-dhcp-client is not optional. The appliance's init only configures eth0
# by asking a DHCP client to do it, so without one the appliance boots with no
# address at all, and every apt lookup in the guest fails with "Temporary
# failure resolving" no matter how healthy the host's network is. It has to be
# installed before the appliance is built, because that step packs these host
# files into the appliance.
RUN apt-get update && apt-get install -y --no-install-recommends \
      libguestfs-tools \
      linux-image-virtual \
      qemu-utils \
      qemu-system-x86 \
      isc-dhcp-client \
  && rm -rf /var/lib/apt/lists/*

# Bake the appliance into the image. Otherwise supermin rebuilds it from the
# package database on every run, which costs a minute a build and makes the
# result depend on whatever apt state the layer happens to carry. Building it
# here also means a broken toolchain fails at image build time rather than
# part way through a disk customization.
RUN libguestfs-make-fixed-appliance /usr/local/lib/guestfs/appliance

ENV LIBGUESTFS_PATH=/usr/local/lib/guestfs/appliance
# The appliance is fixed, so there is nothing for libvirt to manage and the
# direct backend avoids needing a session daemon in the container.
ENV LIBGUESTFS_BACKEND=direct

# Prove the appliance boots and has working networking while building the
# image. Without /dev/kvm this falls back to TCG, which is slow but works. The
# network half matters as much as the boot: an appliance that comes up without
# an address fails much later, deep inside a guest apt run, and looks like a
# problem with the guest rather than with this image.
RUN qemu-img create -f qcow2 /tmp/probe.qcow2 16M \
  && guestfish --network -a /tmp/probe.qcow2 run \
       : debug sh "ip -4 -o addr show eth0 | grep -q 169.254 && grep -q nameserver /etc/resolv.conf && echo APPLIANCE_NETWORK_OK" \
     | grep -q APPLIANCE_NETWORK_OK \
  && rm -f /tmp/probe.qcow2
