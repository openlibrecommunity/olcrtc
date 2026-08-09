{
  description = "olcrtc development environment";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { nixpkgs, ... }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      devShells = forAllSystems (system:
        let
          pkgs = import nixpkgs {
            inherit system;
            config = {
              allowUnfree = true;
              android_sdk.accept_license = true;
            };
          };
          androidSdk = (pkgs.androidenv.composeAndroidPackages {
            platformVersions = [ "35" ];
            buildToolsVersions = [ "35.0.0" ];
            includeNDK = true;
          }).androidsdk;
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              androidSdk
              ffmpeg
              go
              golangci-lint
              gomobile
              jdk17
              mage
            ];

            ANDROID_HOME = "${androidSdk}/libexec/android-sdk";
            ANDROID_NDK_HOME = "${androidSdk}/libexec/android-sdk/ndk-bundle";
            JAVA_HOME = pkgs.jdk17.home;

            shellHook = ''
              export GOCACHE="''${XDG_CACHE_HOME:-$HOME/.cache}/go-build"
              export GOMODCACHE="''${XDG_CACHE_HOME:-$HOME/.cache}/go-mod"
            '';
          };
        });
    };
}
