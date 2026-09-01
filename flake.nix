{
  description = "okf-tools: the okftool and okfpub CLIs for OKF bundles, plus a dev shell bundling qmd (llm-toolchain) from the firefly toolbox";

  inputs.toolbox.url = "github:firefly-engineering/toolbox";

  outputs =
    { toolbox, ... }:
    let
      # Reuse the toolbox's own nixpkgs so llm-toolchain stays a binary-cache
      # hit and the CLIs build against the same pinned toolchain.
      nixpkgs = toolbox.inputs.nixpkgs;
      inherit (nixpkgs) lib;

      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = lib.genAttrs systems;

      # One version binding for the whole module: okftool and okfpub ship
      # lockstep from the same tag (#46), and the release workflow rewrites
      # exactly this line — it fails loudly if it ever finds more than one.
      version = "0.4.0";

      # Recomputed with `nix build .#okftool` (copy the hash from the mismatch).
      # One Go module, so every derivation below vendors identically.
      vendorHash = "sha256-A5fl7XTNNcQciOj1M5U+ZUuB4doXSinHv2vkcbeFgO0=";

      # mkGo builds one derivation out of this module. subPackages scopes both the
      # build and the checkPhase, so a binary package installs only its own command
      # and leaves the test suite to the one derivation that runs it whole (below).
      mkGo =
        system:
        {
          pname,
          subPackages ? null,
          doCheck ? false,
          postInstall ? "",
          meta ? { },
        }:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        pkgs.buildGoModule (
          {
            inherit
              pname
              version
              vendorHash
              doCheck
              postInstall
              meta
              ;
            src = lib.cleanSource ./.;
            ldflags = [
              "-s"
              "-w"
              "-X main.version=${version}"
            ];
          }
          // lib.optionalAttrs (subPackages != null) { inherit subPackages; }
        );

      okftoolFor =
        system:
        mkGo system {
          pname = "okftool";
          # Both cmd/okftool and cmd/okfpub are main packages, so this must be
          # explicit: without it `go install ./...` would put both binaries in
          # every output.
          subPackages = [ "cmd/okftool" ];
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

      okfpubFor =
        system:
        mkGo system {
          pname = "okfpub";
          subPackages = [ "cmd/okfpub" ];
          meta = {
            description = "Publisher that mirrors an Open Knowledge Format bundle to a backend (Notion, filesystem)";
            mainProgram = "okfpub";
          };
        };

      # The module's test suite as a single derivation: no subPackages, so
      # checkPhase runs the full `go test ./...` (parser, rules, publish, and the
      # golden fixture bundles) once rather than once per binary.
      testsFor =
        system:
        mkGo system {
          pname = "okf-tools-tests";
          doCheck = true;
          meta.description = "Full okf-tools Go test suite, run as a flake check";
        };
    in
    {
      packages = forAllSystems (system: {
        okftool = okftoolFor system;
        okfpub = okfpubFor system;
        default = okftoolFor system;
      });

      apps = forAllSystems (system: {
        okftool = {
          type = "app";
          program = "${okftoolFor system}/bin/okftool";
        };
        okfpub = {
          type = "app";
          program = "${okfpubFor system}/bin/okfpub";
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
              (okfpubFor system)
            ];
          };
        }
      );

      # `nix flake check` builds both CLIs and runs the module's test suite.
      checks = forAllSystems (system: {
        okftool = okftoolFor system;
        okfpub = okfpubFor system;
        tests = testsFor system;
      });
    };
}
