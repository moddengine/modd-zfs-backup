{ pkgs ? import <nixpkgs> {} }:

pkgs.mkShell {
  packages = with pkgs; [
    cloud-utils
    coreutils
    curl
    gawk
    gnutar
    go
    jq
    libvirt
    openssh
    qemu
    shellcheck
    util-linux
    virt-manager
  ];
}
