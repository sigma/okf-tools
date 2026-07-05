{
  description = "okf-tools: the okf CLI for OKF bundles, plus a dev shell bundling qmd (llm-toolchain) from the firefly toolbox";

  inputs.toolbox.url = "github:firefly-engineering/toolbox";

  outputs =
    { toolbox, ... }:
    let
      # Reuse the toolbox's own nixpkgs so llm-toolchain stays a binary-cache
      # hit and okf builds against the same pinned toolchain.
      nixpkgs = toolbox.inputs.nixpkgs;
      inherit (nixpkgs) lib;

      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = lib.genAttrs systems;

      version = "0.1.0";

      okfFor =
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        pkgs.buildGoModule {
          pname = "okf";
          inherit version;
          src = lib.cleanSource ./.;
          # Recomputed with `nix build .#okf` (copy the hash from the mismatch).
          vendorHash = "sha256-arnjLHFW0fHy6P93g5GQC9ixEU0ld3eP3T3mNQzB+tg=";
          # Only cmd/okf is a main package, so `go install ./...` yields just the
          # okf binary; leaving subPackages unset lets checkPhase run the full
          # `go test ./...` (parser, rules, and golden fixture bundles).
          doCheck = true;
          ldflags = [
            "-s"
            "-w"
            "-X main.version=${version}"
          ];
          meta = {
            description = "Deterministic CLI for authoring and maintaining Open Knowledge Format bundles";
            mainProgram = "okf";
          };
        };
    in
    {
      packages = forAllSystems (system: {
        okf = okfFor system;
        default = okfFor system;
      });

      apps = forAllSystems (system: {
        okf = {
          type = "app";
          program = "${okfFor system}/bin/okf";
        };
        default = {
          type = "app";
          program = "${okfFor system}/bin/okf";
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
              (okfFor system)
            ];
          };
        }
      );

      # `nix flake check` builds okf and runs its test suite (doCheck).
      checks = forAllSystems (system: {
        okf = okfFor system;
      });
    };
}
