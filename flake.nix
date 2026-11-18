{
  description = "Terminal UI for Agent Substrate";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      version = "0-unstable-" + (self.shortRev or self.dirtyShortRev or "dirty");
    in
    {
      packages = forAll (pkgs: rec {
        ate-tui = pkgs.buildGoModule {
          pname = "ate-tui";
          inherit version;
          src = self;
          # Update after a go.mod change: set lib.fakeHash, run nix build,
          # and copy the hash it reports.
          vendorHash = "sha256-9eGMcxWUfe3+2XAftVQ+GZPt9gz0VY4EApMxw8sQ5sE=";
          subPackages = [ "cmd/ate-tui" ];
          env.CGO_ENABLED = 0;
          ldflags = [
            "-s"
            "-w"
            "-X main.version=${version}"
          ];
          meta = {
            description = "Terminal UI for Agent Substrate";
            homepage = "https://github.com/ate-tooling/ate-tui";
            license = pkgs.lib.licenses.asl20;
            mainProgram = "ate-tui";
          };
        };
        default = ate-tui;
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gopls
            pkgs.goreleaser
          ];
        };
      });
    };
}
