{
  description = "kuport — deliver a node's port to a pod on another node, source address intact";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          # Everything every task calls through `nix develop -c`. Nothing is
          # fetched at shell entry; the flake is the whole toolchain.
          packages = with pkgs; [
            go_1_26
            gopls
            golangci-lint
            kubernetes-controller-tools # controller-gen
            kubectl
            kustomize
            kubernetes-helm # helm
            setup-envtest
            git
          ];
        };
      });
    };
}
