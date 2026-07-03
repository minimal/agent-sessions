with import (builtins.fetchGit {
  # Descriptive name to make the store path easier to identify
  name = "nixos-unstable-jun-2026";
  url = "https://github.com/nixos/nixpkgs/";
  # `git ls-remote https://github.com/nixos/nixpkgs nixos-unstable`
  ref = "refs/heads/nixpkgs-unstable";
  rev = "b5aa0fbd538984f6e3d201be0005b4463d8b09f8";
}) {};
  mkShell {
    nativeBuildInputs = [
      bashInteractive
      go
    ];
  }
