{
  description = "okf-tools: the okftool CLI for OKF bundles, plus a dev shell bundling qmd (llm-toolchain) from the firefly toolbox";

  inputs.toolbox.url = "github:firefly-engineering/toolbox";

  outputs =
    { toolbox, ... }:
    let
      # Reuse the toolbox's own nixpkgs so llm-toolchain stays a binary-cache
      # hit and okftool builds against the same pinned toolchain.
      nixpkgs = toolbox.inputs.nixpkgs;
      inherit (nixpkgs) lib;

      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = lib.genAttrs systems;

      version = "0.1.1";

      okftoolFor =
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        pkgs.buildGoModule {
          pname = "okftool";
          inherit version;
          src = lib.cleanSource ./.;
          # Recomputed with `nix build .#okftool` (copy the hash from the mismatch).
          vendorHash = "sha256-tGQvZsSidf04fciYXI/5OpvG9BKYlnFmdoJmLh+af7Q=";
          # Only cmd/okftool is a main package, so `go install ./...` yields just
          # the okftool binary; leaving subPackages unset lets checkPhase run the
          # full `go test ./...` (parser, rules, and golden fixture bundles).
          doCheck = true;
          ldflags = [
            "-s"
            "-w"
            "-X main.version=${version}"
          ];
          # Also expose the bundled agent skill as a file in the output, so a
          # downstream flake / home-manager can install it from the store
          # (${okftool}/share/okftool/SKILL.md) without running the binary. It is
          # the same markdown go:embed builds into `okftool skill`.
          postInstall = ''
            install -Dm644 internal/command/skill.md "$out/share/okftool/SKILL.md"
          '';
          meta = {
            description = "Deterministic CLI for authoring and maintaining Open Knowledge Format bundles";
            mainProgram = "okftool";
          };
        };
    in
    {
      packages = forAllSystems (system: {
        okftool = okftoolFor system;
        default = okftoolFor system;
      });

      apps = forAllSystems (system: {
        okftool = {
          type = "app";
          program = "${okftoolFor system}/bin/okftool";
        };
        default = {
          type = "app";
          program = "${okftoolFor system}/bin/okftool";
        };
      });

      devShells = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShellNoCC {
            name = "okf-tools";
            packages = [
              toolbox.packages.${system}.llm-toolchain
              toolbox.packages.${system}.yq
              toolbox.packages.${system}.jq
              (okftoolFor system)
            ];
          };
        }
      );

      # `nix flake check` builds okftool and runs its test suite (doCheck).
      checks = forAllSystems (system: {
        okftool = okftoolFor system;
      });
    };
}
