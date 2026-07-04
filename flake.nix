{
  description = "okf-tools dev shell: LLM tooling (qmd) from the firefly toolbox";

  inputs.toolbox.url = "github:firefly-engineering/toolbox";

  outputs =
    { toolbox, ... }:
    let
      # Reuse the toolbox's own nixpkgs so llm-toolchain stays a binary-cache
      # hit and its versions line up with how it was built upstream.
      nixpkgs = toolbox.inputs.nixpkgs;
      inherit (nixpkgs) lib;

      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = lib.genAttrs systems;
    in
    {
      devShells = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShellNoCC {
            name = "okf-tools";
            packages = [ toolbox.packages.${system}.llm-toolchain ];
          };
        }
      );
    };
}
